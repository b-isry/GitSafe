# GitSafe — Phase Plan & Work-State

This file is the authoritative, persistent record of GitSafe's phased implementation
roadmap, work-state, and acceptance criteria. Future sessions MUST read this before
beginning any work so that progress does not depend on conversation context.

Product/visual behavior is specified separately in `DESIGN.md`. This file covers
*what* to build and the current implementation state.

---

## Completed Phases

### Phase 0 — Legacy CLI (prior architecture)
The original `main.go` CLI: scan a root for stale repos, bundle them (via `archiver`),
optionally upload to Google Drive. Restructured during the scale-ready refactor into
`internal/`. The web server (`cmd/server/main.go`) is now the primary interface.

### Phase 1 — GitHub OAuth + persistent state
- `internal/state`: versioned, atomic, JSON `state.Store` (connections, protected
  repos, backup records, backup jobs). NEVER stores secrets — only token references.
- `internal/tokenstore`: encrypted keychain token store.
- `internal/server/github.go`: OAuth login/callback, `/api/connections`, disconnect,
  CSRF/session infra, `StateStore`/`TokenStore` interfaces, `ConfigureCloud`.
- `internal/providers`: GitHub API client (identity, repository listing, clone URL).

### Phase 2 — GitHub repository discovery + protection
- `internal/server/cloud.go`: `/cloud-repositories` page, discovery
  (`GET /api/repositories` with `protected` annotation), protection
  (`POST/DELETE /api/protected-repositories`), listing (`GET /api/protected-repositories`).
- `internal/server/templates/cloud-repositories.html`, `internal/server/static/cloud.js`.
- Persisted `state.ProtectedRepo` records keyed by immutable `githubId`.
- Verified: `gofmt`, `go build`, `go vet`, `go test -count=1 ./...` all green.

---

## Phase 3 — Backup & Job Management (IN PROGRESS)

### Goals
1. **Backup configuration/state** — persist necessary backup state using the existing
   architecture; validate and persist safely. Drive/other cloud and unrelated Phase 2
   refactors are OUT OF SCOPE.
2. **Backup execution** — trigger a backup for a protected repository; reuse the
   existing `archiver` infrastructure (`BundleRemoteRepo`); track success and failure;
   handle partial/failure cases safely.
3. **Job management** — represent backup jobs and lifecycle; track pending/running/
   completed/failed; record timestamps/errors; prevent duplicate/conflicting jobs.
4. **Server/API** — APIs to trigger backups and inspect job/backup status/history;
   follow Phase 2 CSRF/auth/validation/error conventions.
5. **UI** — backup controls/status in the cloud/protected-repository workflow;
   loading/empty/success/error states; follow `DESIGN.md`, no unrelated visual changes.
6. **Tests** — focused unit tests for job/backup logic; handler/API tests for
   validation, authorization, failures, and successful orchestration where testable
   without external services; reuse existing seams, avoid new seams solely for coverage.

### Out of scope
- Google Drive integration / other cloud providers.
- Unrelated UI redesign.
- Refactoring unrelated Phase 2 code.

### Acceptance criteria
- A protected repository can be backed up through the intended application workflow.
- Backup jobs have observable lifecycle/status information.
- Backup success and failure are persisted/reported correctly.
- Invalid, unauthorized, duplicate, and failure scenarios are handled safely.
- UI exposes the backup workflow and status consistently with the existing design.
- Tests cover core backup/job logic and important API paths.
- `gofmt -l` is clean; `go build ./...`, `go vet ./...`, `go test -count=1 ./...` pass.
- `git diff`/`git status` contain only intended changes.

### Architecture mapping & design decisions
- **Data model**: reuse existing, all-`state`-side types already persisted:
  - `state.BackupJob` — `ID`, `ProtectedRepoID`, `FullName`, `State`, `StartedAt`,
    `FinishedAt`, `Error`. Lifecycle: `enqueued → cloning → bundling → recording →
    completed` (Drive `uploading` is skipped — out of scope) or `→ failed`.
  - `state.BackupRecord` — terminal success record keyed to a `ProtectedRepo`.
- **StateStore interface extension** (`internal/server/github.go`): add
  `BackupJobs()`, `CreateBackupJob(BackupJob)`, `UpdateBackupJob(BackupJob) error`,
  `BackupJob(id) (BackupJob, bool)`, `UnfinishedJobs()`, `AddBackupRecord(BackupRecord)`,
  `BackupRecords()`, `BackupRecordsForRepo(id)`. Implement on `*state.Store`
  (methods already exist) and extend the test `fakeStateStore`.
- **Execution**: clone+bundle a protected GitHub repo via `archiver.BundleRemoteRepo`
  with a runtime-only tokenized clone URL (`https://x-access-token:<TOKEN>@github.com/<fullName>.git`).
  The tokenized URL is NEVER persisted (satisfies the state "no secrets / no tokenized
  clone URL" rule). Bundle output goes to `app.OutputPath` (same artifacts dir as the
  local backup flow).
- **Async job model**: a detached goroutine (like Phase 0 `createJob`/`runBackup`) walks
  the job through state, updating the persisted job after each phase and `Save()`ing.
  On success it appends a `BackupRecord`; on failure it marks the job `failed` with a
  sanitized message.
- **Duplicate prevention**: refuse to start a job for a protected repo that already has
  a non-terminal (`UnfinishedJobs`) job.
- **Seam**: a `bundleBackup` function-field on `Server` (mirrors the existing
  `connectGitHub` test seam) that wraps `archiver.BundleRemoteRepo`; tests inject a fake
  to exercise orchestration without `git`/network. `BackupRecord` metadata (branch/tag
  counts, head SHAs) is left best-effort/empty this phase since `BundleRemoteRepo`
  discards the mirror; `BundleSize` and `BundleSHA256` are computed from the bundle file.
- **New file**: `internal/server/backupjobs.go` (handlers + orchestration), extension to
  `cloud.js`/`cloud-repositories.html` for the UI, tests in `internal/server/backupjobs_test.go`.

### API contract (new)
- `POST /api/protected-repositories/{id}/backup` — trigger backup (loopback + cloudReady
  + session + CSRF). Body: `{}` (or empty). `202` → `{"job": {...}}`; `409` if a job for
  that repo is already running; `404` unknown protected repo.
- `GET /api/backup-jobs/{id}` — poll a job (loopback). `200` → `state.BackupJob`; `404`.
- `GET /api/protected-repositories/{id}/backup-jobs` — jobs for a repo (loopback).
- `GET /api/protected-repositories/{id}/backups` — backup history (records) for a repo
  (loopback).

### Verification
`gofmt -l` clean; `go build ./...`; `go vet ./...`; `go test -count=1 ./...`.

### Phase 3 status
- [x] Persist scope/plan (this file)
- [x] Server orchestration + seams (`internal/server/backupjobs.go`)
- [x] Handlers + routes (register in `server.Routes()`)
- [x] UI (`cloud-repositories.html` + `cloud.js`: backup buttons, status chips, history)
- [x] Tests (`backupjobs_test.go`, `state_test.go` additions)
- [x] Verification + `git diff`/`git status` review

### Phase 3 files changed
- `internal/server/backupjobs.go` (new) — `bundleBackupFunc` seam, `defaultBundleBackup`,
  `startProtectedBackup`/`runProtectedBackup` orchestration, `finishProtectedJob`, handlers
  (`POST .../backup`, `GET /api/backup-jobs/{id}`, `GET .../backup-jobs`, `GET .../backups`),
  response shaping (`toJobView`, `jobView`, `backupsView`).
- `internal/server/backupjobs_test.go` (new) — handler + orchestration tests.
- `internal/server/server.go` — `Server.bundleBackup` + `Server.jobMu` fields (default bundler set
  in `New`), Phase 3 route registration, `sync` import.
- `internal/server/github.go` — `StateStore` interface extended (`ProtectedRepo(id)`, `BackupJobs`,
  `CreateBackupJob`, `UpdateBackupJob`, `BackupJob(id)`, `UnfinishedJobs`, `AddBackupRecord`,
  `BackupRecords`, `BackupRecordsForRepo`).
- `internal/server/github_test.go` — `fakeStateStore` implements the extended interface with a
  `sync.Mutex` (thread-safe for async orchestration tests).
- `internal/state/state.go` — added `BackupJobs()` plural accessor.
- `internal/state/state_test.go` — `TestBackupJobsAccessors`.
- `internal/server/templates/cloud-repositories.html` — backup toolbar, per-repo backup column + status,
  recent-backups history panel.
- `internal/server/static/cloud.js` — backup trigger/poll, status chips, history rendering, "Backup all".
- `PLAN.md` — this work-state document.

### Phase 3 verification results (all pass)
- `gofmt -l .` — clean
- `go build ./...` — success
- `go vet ./...` — clean
- `go test -count=1 ./...` — all packages `ok`
- `node --check internal/server/static/cloud.js` — valid (syntax)
- `git status`/`git diff` — only intended changes (all within the uncommitted v2 server/state tree
  plus this PLAN.md).

### Phase 3 known limitations
- Bundle output is written to `app.OutputPath` (the same local artifacts dir as the legacy local
  backup flow). Google Drive upload is a later phase, so `BackupRecord.Status` is `"bundled"`
  (not `"uploaded"`) and jobs stop at `recording → completed` (no `uploading` state).
- `BackupRecord` ref-metadata (`HeadSHAs`, `BranchCount`, `TagCount`, `DefaultHead`) is left empty
  because `archiver.BundleRemoteRepo` discards the temporary mirror. `BundleSize` and `BundleSHA256`
  are computed from the bundle file.
- Backup jobs run detached in a goroutine; on shutdown, unfinished jobs are normalized to
  `interrupted` by the state store on next `Open` (existing behavior). No resume/retry this phase.
- Concurrent trigger of the SAME repo is prevented via `jobMu` + `UnfinishedJobs`; a job started and
  then a second request during a running job returns 409.

## Phase 4 — Google Drive Integration (IN PROGRESS)

### Scope
Extend the completed local backup workflow so completed bundles can be uploaded to
Google Drive and tracked through the existing `state.BackupJob`/`BackupRecord`
architecture.

### Goals
1. **Drive connection** — reuse the existing `state.DriveConnection` model; report
   configured/connected/usable status safely; never store credentials secrets in state.
2. **Drive upload** — upload the completed local bundle; preserve the local bundle even
   when upload fails; capture the resulting Drive file ID; persist
   `BackupRecord.DriveFileID` only after a successful upload.
3. **Job lifecycle** — add an `uploading` state so the flow reads
   `enqueued → cloning → bundling → recording → uploading → completed`. A failed upload
   transitions the job to `failed` (never silently `completed`). Drive disabled/not
   configured still completes locally (`bundled`), consistent with the product design.
4. **Server/API** — expose Drive connection/status and drive-upload state via the existing
   `GET /api/connections` and job/history endpoints; follow Phase 2–3 auth/CSRF/error
   conventions; no unrelated API redesign.
5. **UI** — extend the existing cloud/backup UI: show Drive connection/config status and a
   per-backup state that distinguishes local-only (`bundled`), `uploading`, `uploaded`, and
   `failed`; loading/success/empty/error states per `DESIGN.md`.
6. **Testing** — a minimal Drive-upload seam so tests need no real Google credentials or
   network; cover success, disconnected/unavailable, auth/token failure, upload failure,
   `DriveFileID` persistence, `uploading → completed` and failure-during-`uploading`, local
   preservation after Drive failure, and handler auth/CSRF/validation paths. Existing Phase
   2–3 tests stay green.

### Out of scope
- Dropbox/S3/OneDrive/other providers.
- Backup scheduling/retry automation (not already required).
- Major UI redesign.
- Rewriting the encryption/token-storage architecture.
- Unrelated refactoring.

### Acceptance criteria
- A completed local backup can be uploaded to Google Drive.
- Successful uploads persist the corresponding `DriveFileID`.
- The job lifecycle exposes the Drive upload phase.
- Upload failures are observable and do not destroy the local bundle.
- Missing/disconnected/invalid Drive credentials are handled safely.
- Auth/authz/CSRF protections remain intact.
- UI reflects Drive and backup state accurately.
- Tests need no live Google credentials/network.
- `gofmt -l` clean; `go build ./...`, `go vet ./...`, `go test -count=1 ./...` pass;
  `node --check` passes on modified JS; `git status`/`git diff` only intended changes.

### Architecture mapping & design decisions
- **Credential mechanism (inspected):** the only existing upload path is
  `internal/cloud/drive.go` → `cloud.UploadFile(ctx, config.CloudConfig, bundle, ...)` using a
  service-account JWT from `config.CloudConfig.CredentialsFile` (scope `DriveFileScope`).
  This is the established mechanism. Per scope ("do not introduce a second/competing
  credential-storage mechanism"), Phase 4 reuses it rather than building an OAuth user flow.
  The upload target is the service account's Drive (no personal-folder OAuth).
- **`DriveConnection` reuse:** used as the persisted connection record
  (`AccountEmail`, `ConnectedAt`, `StorageFolderID`). For service-account JWT there is no
  keychain token, so `TokenRef` stays empty — a documented limitation of reusing the model
  with the file-based mechanism (the encrypted token-ref path would be exercised only by a
  future OAuth flow).
- **Drive-upload seam:** `driveUploadFunc` function-field on `Server` (mirrors
  `bundleBackup`/`connectGitHub`). Production default wraps `cloud.UploadFile`; tests inject a
  fake so no Google credentials/network are needed.
- **Where upload fits:** in `runProtectedBackup`, after `recording` and before `completed`.
  Drive is considered enabled when `s.app.Config.Cloud.Enabled && CredentialsFile != ""`.
  - Enabled + success → job `uploading → completed`; `BackupRecord.Status="uploaded"`,
    `DriveFileID` set; local bundle kept.
  - Enabled + failure → job `failed` with a sanitized error; local `BackupRecord` kept
    with `Status="bundled"` (local preserved).
  - Disabled/not configured → job completes locally; `BackupRecord.Status="bundled"`.
- **Connection API:** extend `GET /api/connections` `drive` block to report
  `configured` (from `Config.Cloud`) and `connected` (configured + token-fetch/credentials
  reachable) instead of the hard-coded `false/false`.
- **Files:** extend `internal/server/backupjobs.go` (seam + upload step), `github.go`
  (`handleAPIConnections` drive block), `cloud.go`/`cloud.js`/template for status, tests in
  `internal/server/backupjobs_test.go` + `backupjobs_drive_test.go`.

### Phase 4 status
- [x] Inspect existing Drive/config/token/upload code
- [x] Persist scope/plan (this section)
- [x] Drive-upload seam + job `uploading` state
- [x] Connection API status
- [x] UI
- [x] Tests
- [x] Verification + `git status`/`git diff` review
- [x] Update work-state with results/limitations

### Phase 4 files changed
- `internal/server/backupjobs.go` — added `driveUploadFunc` seam + `defaultDriveUpload`
  (wraps `cloud.UploadFile`), `Server.driveEnabled()`, `uploading` job state between
  `recording` and `completed` in `runProtectedBackup`; `BackupRecord` upgraded to
  `uploaded` + `DriveFileID` after a successful upload; `upstream` preserved as
  `bundled` (local-only) on upload failure; `backupView` now exposes `DriveFileID`.
- `internal/server/server.go` — `Server.driveUpload` field + default wiring in `New`.
- `internal/server/github.go` — `StateStore` interface gained `UpdateBackupRecord`;
  `handleAPIConnections` drive block now reports `configured`/`connected` from
  `Config.Cloud`.
- `internal/state/models.go` — added `BackupStatusBundled/Uploaded/Failed` constants.
- `internal/state/state.go` — added `Store.UpdateBackupRecord`.
- `internal/state/state_test.go` — (none added; existing coverage unaffected).
- `internal/server/github_test.go` — `fakeStateStore` implements `UpdateBackupRecord`
  (honors `errors["updateRecord"]`).
- `internal/server/backupjobs_drive_test.go` (new) — Drive upload success/failure/
  skip, `uploading` reachability, record-update error tolerance, connections API
  drive status.
- `internal/server/templates/cloud-repositories.html` — Drive status indicator.
- `internal/server/static/cloud.js` — `backupStatusChip` (uploaded/bundled/failed) in
  history, `loadDriveStatus`, wired into `loadAll`.
- `PLAN.md` — this work-state document.

### Phase 4 architecture note (credential reconciliation)
The existing and only upload path is a service-account JWT from
`config.CloudConfig.CredentialsFile` (`cloud.UploadFile`). Per scope (no competing
credential mechanism), Phase 4 reuses it. `state.DriveConnection.AccountEmail`,
`ConnectedAt`, and `StorageFolderID` are carried by the model but are not populated
by the service-account flow (no keychain token, no per-user folder); `TokenRef`
stays empty. Drive is regarded as "configured/connected" when
`cloud.enabled && credentialsFile != ""`, surfaced via `/api/connections`. The
encrypted token-ref path would be exercised only by a future OAuth flow.

### Phase 4 verification results (all pass)
- `gofmt -l .` — clean
- `go build ./...` — success
- `go vet ./...` — clean
- `go test -count=1 ./...` — all packages `ok`
- `node --check internal/server/static/cloud.js` — valid
- `git status`/`git diff` — changes confined to the uncommitted v2 tree + PLAN.md

### Phase 4 known limitations
- Drive upload runs once per backup inside `runProtectedBackup`; with the
  service-account (non-OAuth) mechanism there is no per-user folder or web
  "connect" button, so `DriveConnection` metadata/token-ref remain unpopulated.
- Upload progress is not streamed to the UI; jobs/polling surface only the
  `uploading` lifecycle state.
- A Drive-upload failure fails the job and keeps the local bundle (`bundled`);
  there is no automatic retry this phase.
- Does not cover Dropbox/S3/OneDrive; those remain future work.

---

## Phase 5 — End-to-End Hardening & Integration (IN PROGRESS)

### Scope
Production-safe hardening of the complete GitHub → protected repo → backup job →
local bundle → Drive upload workflow. No new major feature; fix only issues
discovered by a full-lifecycle review. See task prompt for the 8 work areas.

### Review findings (recorded before fixes)
Tracing `startProtectedBackup`/`runProtectedBackup`, `archiver.BundleRemoteRepo`,
`state.Store`, the async goroutine, and the connections/settings API surfaced:

1. **SECURITY – token leak into persisted state (critical).**
   `defaultBundleBackup` builds `https://x-access-token:<TOKEN>@github.com/...`,
   passes it to `archiver.BundleRemoteRepo`, whose clone error is
   `"mirror clone %q: ... (%s)"` and embeds the full tokenized URL. That error
   flows to `finishProtectedJob` → `BackupJob.Error` → persisted `state.json` and
   the `GET /api/backup-jobs/{id}` response. Violates "state never stores secrets".
   Fix: redact/blind the URL in `archiver` errors and sanitize the persisted
   message (strip `scheme://user:pass@`), plus a regression test.
2. **Correctness – bundle filename collision.** `BundleRemoteRepo` names bundles
   `<repo>_<20060102_150405>.bundle` (second resolution). Two sequential backups of
   the same repo in the same second overwrite the earlier file while the older
   `BackupRecord` still references that name/hash → silent data loss + corrupt
   record. Fix: unique suffix per bundle (+test).
3. **Cleanup – orphaned partial bundle on failure.** If `git bundle create` fails
   after the temp clone succeeds, any partially-written `.bundle` is left in
   `OutputPath` and later counted by `App.Backups()`. Fix: remove the bundle file on
   error in `BundleRemoteRepo`.
4. **Recovery – orphaned non-terminal job on persistence failure.**
   `startProtectedBackup` calls `CreateBackupJob` then `saveState`; if save fails it
   returns 500 but leaves an `enqueued` non-terminal job that 409s that repo until
   restart. Fix: remove the just-created job on save failure (add `RemoveBackupJob`).
5. **RACE (async vs settings) – backup goroutine reads unsynchronized App config.**
   `runProtectedBackup` runs `go s.runProtectedBackup(...)` and reads
   `s.app.OutputPath` / `s.app.Config.Cloud` (in `driveEnabled`) without holding
   `App.mu`, while `handleSaveSettings`/`ApplySettings` swap those fields — a data
   race detectable by `-race`. Fix: snapshot config at job start under `App.mu` and
   pass it into the goroutine.
6. **Concurrency – confirmed safe but untested.** `jobMu` + `UnfinishedJobs` dedup is
   correct; `state.Store` (real) and `fakeStateStore` (tests) are mutex-guarded.
   Add focused concurrency regression tests (concurrent duplicate triggers, read
   during async run) and note `-race` result.
7. **Drive consistency – verified correct.** `DriveFileID` set + `Status="uploaded"`
   only after `driveUpload` returns success; `bundled` = local-only; failed upload
   keeps `bundled` + local file and fails the job; Drive-disabled runs complete
   locally. No code change (kept tests).
8. **API/UI consistency – verified.** `jobLabel`/`backupStatusChip` map terminal and
   running states to backend values; no stale-state found. No change.

Secure/narrow fixes are applied for items 1-5; 6 adds tests; 7-8 documented only.

### Out of scope
New providers, OAuth redesign, scheduling, automatic retry, UI redesign,
unrelated features, large refactors.

### Files to change
- `internal/archiver/archiver.go` — redact URL in errors; unique bundle name; clean
  up partial bundle on failure.
- `internal/server/backupjobs.go` — snapshot output path + cloud cfg at job start;
  pass into `runProtectedBackup`; sanitize persisted failure message; remove orphan
  job on save failure.
- `internal/state/state.go` + `github.go` (StateStore) + `github_test.go`
  (fakeStateStore) — add `RemoveBackupJob`.
- `internal/server/app.go` — add lock-guarded config snapshot accessor.
- Tests: `internal/archiver/archiver_test.go` (new), additions to
  `internal/server/backupjobs_test.go`/`backupjobs_drive_test.go`, concurrency tests.
- `PLAN.md` — this document.

### Phase 5 status
- [x] Read PLAN.md + inspect full Phase 2-4 implementation / lifecycle
- [x] Document review findings before fixes (above)
- [x] Apply fixes 1-5
- [x] Add regression + concurrency tests
- [x] Verification (`gofmt`, `build`, `vet`, `test`, `node --check`, `-race` attempted)
- [x] Update PLAN.md with results/limitations/next phase

### Phase 5 fixes applied
1. **Security — no token leak into persisted state or API.**
   - `archiver.BundleRemoteRepo` now redacts credentials from the clone URL
     (`redactCloneURL`) in all error messages; `uniqueSuffix()` names bundles and
     partial bundles are removed on `git bundle create` failure.
   - `server.finishProtectedJob` sanitizes the persisted/returned message with
     `sanitizeMessage` (strips `scheme://user:pass@`), so even an unforeseen error
     path cannot write a tokenized URL into `BackupJob.Error`/state.
2. **Correctness — bundle filenames are unique.** `uniqueSuffix` appends a
   crypto-random component so two sequential backups of the same repo cannot
   overwrite each other's file (which previously could corrupt an older record
   that still referenced the filename + SHA256).
3. **Cleanup — no orphaned partial bundle.** `BundleRemoteRepo` removes any
   partially-written `.bundle` when the bundle step fails.
4. **Recovery — no orphaned non-terminal job on persistence failure.**
   `startProtectedBackup` removes the freshly-created job when the initial
   `saveState()` fails (new `state.Store.RemoveBackupJob`),
   so a retry is not blocked by a 409 until restart.
5. **Race — async goroutine reads a config snapshot.** `runProtectedBackup` reads
   `s.app.ConfigSnapshot()` once under the app lock before doing any work, so a
   concurrent settings save cannot race with the detached backup goroutine reading
   `App.OutputPath`/`App.Config.Cloud`.
6. **Concurrency — confirmed safe and now tested.** `jobMu` + `UnfinishedJobs`
   dedup, and locked stores, verified by new concurrent-trigger and read-while-
   running regression tests.

### Phase 5 files changed
- `internal/archiver/archiver.go` — `redactCloneURL`, crypto-random `uniqueSuffix`,
  partial-bundle cleanup on failure.
- `internal/archiver/archiver_test.go` (new) — redaction + uniqueness tests.
- `internal/server/backupjobs.go` — config snapshot in `runProtectedBackup`,
  `driveEnabledFor` (value-based), `sanitizeMessage`/`tokenURLPattern` applied in
  `finishProtectedJob`, orphan-job cleanup in `startProtectedBackup`.
- `internal/server/app.go` — `App.ConfigSnapshot()` (lock-guarded).
- `internal/state/state.go` + `github.go` (StateStore) + `github_test.go`
  (fakeStateStore) — `RemoveBackupJob`.
- `internal/state/state_test.go` — `TestBackupJobRemove`.
- `internal/server/backupjobs_hardening_test.go` (new) — token-sanitization,
  orphan-job cleanup, concurrent-trigger dedup, read-while-running safety.

### Phase 5 verification results
- `gofmt -l .` — clean
- `go build ./...` — success
- `go vet ./...` — clean
- `go test -count=1 ./...` — all packages `ok`
- `node --check internal/server/static/cloud.js` — valid (no JS changed this phase)
- `go test -race` — NOT runnable in this environment: `-race requires cgo`, and no
  C compiler (`gcc`/`cc`/`clang`) is installed, so cgo cannot build. This is an
  **environmental limitation**, not a failure of the implementation. The concurrency
  paths were reviewed for locking and covered by dedicated non-race regression tests.

### Phase 5 remaining limitations / deferred
- Legacy local-backup flow (`backup.Runner` → `archiver.BundleRepo`) still uses a
  second-resolution bundle name and could, in theory, overwrite a same-second
  duplicate for the SAME local repo. Out of Phase 3-5 scope; noted for a future
  sweep.
- Race detector remains unavailable until a C toolchain is installed on the dev
  machine; run `go test -race` once `gcc`/clang exists.
- `DriveConnection` metadata/token-ref still unpopulated (service-account flow, per
  Phase 4 limitation).
- No automatic retry/resume of failed or interrupted jobs (by design, out of scope).

## Phase 6 — Retention, Cleanup & Storage Lifecycle (COMPLETE)

### Scope
Add a deterministic retention/cleanup system over the Phase 2-5 workflow
(GitHub → protected repo → job → local bundle → optional Drive upload → persisted
record/job state) so local bundles, backup records, jobs, orphans, and (optionally,
explicitly) Drive copies do not grow without bound. Cleanup is manual, idempotent,
restart-safe, concurrency-aware, and defaults to **refusing to delete** unless a
retention policy explicitly opts in.

### Goals / mapping to the prompt
1. **Retention config** — new `config.RetentionConfig` on `Config`; conservative
   defaults (all `0`/`false` = keep everything). Reused/parsed by settings Save +
   Validate so unsafe values are rejected before they can cause destructive behavior.
2. **Local retention** — a retention engine computes which recorded local bundles and
   which orphan bundles are eligible, per a per-repo keep-count and/or keep-age policy.
3. **Backup-record consistency** — after local deletion, a record whose Drive copy
   remains stays `uploaded` (DriveFileID intact); a local-only (`bundled`) record whose
   bundle is deleted is itself removed; a reconciliation pass removes records that
   point to a missing local bundle AND no Drive copy. Uses existing statuses; no new
   state added.
4. **Drive retention** — separate and opt-in (`DriveRetentionEnabled` + `KeepDriveDays`).
   Local deletion NEVER auto-deletes the Drive copy. Only files by our own persisted
   `DriveFileID` are targeted; deletion is reported only when the provider confirms.
   Reuses the Phase 4 service-account JWT architecture (new `cloud.DeleteFile`, no new
   auth mechanism). After a confirmed Drive delete, the record is downgraded to
   `bundled` + `DriveFileID` cleared.
5. **Stale/orphan artifacts** — orphan bundle cleanup is restricted to files matching the
   v2 protected-bundle naming convention (`<prefix>_<timestamp>_<hex>.bundle`, verified by
   regex) AND unreferenced by any record AND whose owning repo has no unfinished job.
   Unknown/legacy `.bundle` files not matching the v2 pattern are untouched. Record↔file
   reconciliation (above) handles "records pointing to missing files."
6. **Job-history cleanup** — `KeepJobs` retains the newest N *terminal* jobs
   (completed/failed/interrupted) per repo; non-terminal (active) jobs are never removed.
   Job removal is decoupled from bundle lifecycles (records own bundles).
7. **Concurrency/safety** — the engine is serialized by a dedicated `cleanupMu` (so two
   manual triggers do not double-run) and reads a job snapshot taken under the existing
   `jobMu` (atomic with job creation). Records/files belonging to a repo with an
   unfinished job are always skipped. Re-verification happens before deletion. This
   closes the "discover A eligible → A becomes active → delete A" class: protected
   backups use unique bundle filenames (Phase 5), so a new backup never reuses an
   eligible record's name; and orphan/record deletion is gated on no unfinished job.
   No broad global lock blocks normal backup operations.
8. **Cleanup service boundary** — a testable `internal/retention` engine (pure planning +
   injected fsops/drive/active callbacks) returns a `Result` with inspected/retained/
   deleted/skipped/missing/orphan/jobs/errors breakdown. Server is a thin adapter.
9. **Server/API** — `GET /api/retention` (current policy + last result), `PUT
   /api/retention` (update policy; loopback + session + CSRF + validation), `POST
   /api/retention/cleanup` (manual trigger; loopback + session + CSRF; returns Result).
   Responses expose counts/bundle filenames/Drive ids, never raw filesystem paths.
10. **UI** — retention numeric settings on the Settings page (persisted via the existing
    config Save); a "Storage retention" panel + "Run cleanup" trigger + last-result
    summary on the cloud-repositories page. Follows `DESIGN.md` and existing cloud/backup
    UI; no unrelated redesign.

### Out of scope
Scheduled/cron cleanup, automatic backup scheduling, automatic retry infrastructure,
Dropbox/OneDrive/S3/other providers, OAuth redesign, major UI redesign, rewriting
encryption/token storage, refactoring the legacy local `BundleRepo` flow. Cleanup is a
deterministic service a later scheduling phase could drive.

### Retention policy model
```
retention:
  keepLocal: 0            # max local backups (records+bundles) per repo; 0 = unlimited
  keepLocalDays: 0        # max age in days of a local backup; 0 = unlimited
  keepJobs: 0             # max terminal jobs retained per repo; 0 = unlimited
  keepDriveDays: 0        # min age (days) a Drive copy must be to be deletable; 0 = never
  driveRetentionEnabled: false  # must be true for ANY Drive deletion
```
Defaults keep everything (no unintended destructive behavior). A local bundle/record is
locally eligible when it is beyond the keep-count cap OR older than keepLocalDays. A
record is retained (not deleted) if it still has a Drive copy.

### Validation (config.Validate + settings)
- Reject negative values for all retention numeric fields (0 = off / unlimited).
- Reject `driveRetentionEnabled=true` without `keepDriveDays>0` (a Drive policy that
  could never match, or one that deletes unconditionally, is refused). Conservative.

### Architecture mapping
- **New package** `internal/retention` — pure, injectable engine (no HTTP/state-store
  dependency beyond `state` types): `Policy`, `Policy.Validate`, `LocalBundle`,
  `FSOps` (list/remove local bundles), `DriveDeleter`, `IsActiveRepo`, `Engine.Run`,
  `Result`, `PlanLocal`, `PlanJobs`, `IsOrphanCandidate`. Exhaustively unit-tested.
- **`internal/config/config.go`** — `RetentionConfig` on `Config`; `Defaults`, `Save`
  (persist), `Validate`.
- **`internal/cloud/drive.go`** — add `DeleteFile(ctx, cloudCfg, fileID, logger)` using
  the service-account JWT (reuse). New `Server.driveDeleteFunc` seam (test stub).
- **`internal/server/cleanup.go`** (new) — `Server.runCleanup`, the three API handlers,
  policy view/result shaping, `cleanupMu`; `driveDeleteFunc` seam default.
- **`internal/server/settings.go` + settings template** — retention fields in
  `settingsInput`/`SettingsView`/`validateSettings`/`handleSaveSettings` + `config.Save`.
- **`internal/server/server.go`** — register retention routes, add `server.driveDeleteFunc`
  field + default in `New`.
- **`internal/server/static/cloud.js` + `cloud-repositories.html`** — retention panel +
  cleanup trigger + result.
- **`internal/state`** — reuse `UpdateBackupRecord`, `RemoveBackupJob`, `BackupRecords`,
  `BackupJobs`, `UnfinishedJobs`; **no schema change** (statuses already model remote /
  local-only / missing).

### Phase 6 status
- [x] Read PLAN.md + inspect full Phase 2-5 implementation
- [x] Persist scope/plan (this section)
- [x] Retention config (model + defaults + validation + Save)
- [x] `internal/retention` engine + unit tests
- [x] Drive deletion (`cloud.DeleteFile` + `driveDeleteFunc` seam)
- [x] Server handlers + routes + result shaping
- [x] UI (retention panel + cleanup trigger on cloud-repositories page)
- [x] Server-side integration + concurrency + idempotency tests
- [x] Verification (`gofmt`, `build`, `vet`, `test`, `node --check`, `-race` attempted)
- [x] Update work-state with results/limitations

### Phase 6 results, limitations & notes
Implemented and verified (all green):
- **Retention config** — `config.RetentionConfig` (`keepLocal`, `keepLocalDays`,
  `keepJobs`, `keepDriveDays`, `driveRetentionEnabled`) with all-zero/false defaults;
  `Config.Validate()` rejects negatives and `driveRetentionEnabled` without `keepDriveDays>0`;
  persisted through `config.Save`/`Load`.
- **Engine** — `internal/retention` (pure, injectable `FSOps`/`DriveDeleter`/
  `IsActiveRepo`/`IsActiveName`): per-repo keep-count and keep-age for local bundles,
  record-removal for local-only records losing their bundle, record-keep for records with a
  Drive copy, `reconcileRecords` (remove records with no Drive copy pointing at a missing
  bundle), orphan cleanup restricted to the v2 bundle regex `^(.+)_\d{8}_\d{6}_[0-9a-f]{8}\.bundle$`,
  job-history retention of the newest N terminal jobs, confirmed-only Drive deletion with
  record downgrade to `bundled`. Disabled policy runs delete nothing.
- **Drive deletion** — `cloud.DeleteFile` (service-account JWT, confirmed-only success);
  `Server.driveDeleteFunc` seam + `defaultDriveDelete`, wired in `New`.
- **State** — added `Store.RemoveBackupRecord` + `StateStore.RemoveBackupRecord`; no schema
  change (reuses `bundled`/`uploaded`).
- **Server/API** — `GET /api/retention`, `PUT /api/retention`, `POST /api/retention/cleanup`
  (loopback + session + CSRF + validation; results/counts only, no raw paths). `runCleanup`
  is serialized by `cleanupMu`, snapshots jobs under `jobMu`, skips repos with unfinished
  jobs, tolerates a nil/unconfigured `stateStore` (returns an empty run), and applies state
  mutations (record removal before downgrade).
- **UI** — retention panel ("Keep local backups", "or days", "Keep job history", "Drive
  copies older than", "Delete Drive copies too", Save + Run cleanup now + last-result
  summary) on the cloud-repositories page; no paths exposed.
- **Tests** — 13 retention-engine unit tests (by-count, by-age, both, uploaded-kept,
  local-only-removed, missing bundle reconcile, already-deleted, orphan v2-only + referenced
  kept, job history incl. active, active-repo skip, active-name skip, idempotency, drive
  confirmed/failed/disabled) + `DiskFS` tests; 10 server integration tests (GET/PUT/cleanup
  handlers, auth/CSRF, validation, eligible deletion, active-job skip, drive confirm +
  downgrade, drive failure, disabled policy, concurrent serialized runs).
- **Verification** — `gofmt -l .` clean; `go build ./...` OK; `go vet ./...` OK;
  `go test -count=1 ./...` all packages pass; `node --check internal/server/static/cloud.js` OK.
- **Limitations** — `go test -race` cannot run on this Windows box (`-race requires cgo`,
  no C toolchain), same environmental limitation as Phase 5; the concurrent-cleanup test
  still exercises serialized behavior but not under the race detector. Drive retention
  requires the Phase 4 service-account credentials to be configured; until then the UI shows
  "Drive is not configured". Cleanup is manual only (scheduling is out of scope).

### Next phase (proposal)
A scheduling/orchestration phase that drives the already-deterministic retention service on
a timer/cron basis (e.g. `runCleanup` after each backup and on startup), plus optional
notifications. Alternatively, encryption at rest of local bundles or a restore workflow.

### Verification plan
`gofmt -l .` clean; `go build ./...`; `go vet ./...`; `go test -count=1 ./...`;
`node --check` on modified JS; `git status`/`git diff` only intended changes;
`go test -race` attempted (document environmental limitation if cgo unavailable).

---

## Phase 7 — Scheduling & Notifications (COMPLETE)

### Scope
Turn Phase 6's manual retention cleanup into an optional operational system: a
testable scheduler that periodically calls the **existing** `runCleanup` engine
(it is NOT a rewrite of Phase 6 — the scheduler decides *when* to clean, never
*how*), with run-status observability, a minimal notifier abstraction, API/UI
extensions for the "Scheduled Cleanup" section, and comprehensive deterministic
tests. Scheduling is **off by default** so old config files (without the new
fields) behave exactly as before.

### Goals / mapping to the prompt
1. **Config** — `Config.ScheduledCleanupEnabled bool` + `Config.ScheduledCleanupIntervalDays int`
   (names per prompt; type/unit follows existing int-day retention conventions, confirmed
   with the user). Defaults: disabled + `0`. `Validate()` rejects `ScheduledCleanupIntervalDays <= 0`
   when enabled (negatives are refused like other retention numeric fields). Persisted through
   the existing `Save`/`Load` (backward compatible — files without the field load disabled).
2. **Scheduler** — new `internal/schedule` package (mirrors `internal/retention`): DI +
   injectable clock/ticker, `Start()`/`Stop()`, skip-if-overlapping (a tick that fires while
   the previous run is still active is coalesced/recorded, never runs concurrently), and clean
   shutdown (no goroutine leaks, no app hang). No heavy third-party scheduler dependency.
3. **Status model** — in-memory status: enabled, interval, running, last start/completion,
   last result (reuse `retention.Result`), last trigger (`manual|scheduled`), success/failure,
   a basic summary, and `nextRun` when determinable. Distinguishes manual vs scheduled runs.
4. **Notifier** — `Notifier` abstraction (e.g. `NotifyCleanupResult(ctx, result, summary)`) with
   a structured-logging implementation. A notification failure NEVER fails the cleanup.
5. **API** — extend the existing `GET/PUT /api/retention` (no new endpoints): GET returns the
   scheduling/status block; PUT accepts `scheduledCleanupEnabled` + `scheduledCleanupIntervalDays`,
   validates, persists, and cleanly restarts/replaces the running scheduler on change. Loopback,
   session, CSRF all preserved.
6. **UI** — "Scheduled Cleanup" section in the cloud-repositories retention panel: enable
   checkbox, interval (days) input, save, active indicator, last result, next run; degrades
   gracefully when cloud isn't configured. Extend `cloud.js` (keep using `num()` / flash helpers).
7. **Lifecycle** — add graceful shutdown to `cmd/server/main.go` (signal handling +
   `http.Server.Shutdown` + scheduler `Stop`), satisfying "clean shutdown, no leaks, no hang".
8. **Tests** — deterministic, fake-clock based (no `time.Sleep`/flaky time):
   - `internal/schedule` unit tests: disabled, enabled interval, stop clean/leaks, no concurrent
     exec, coalesced overlapping ticks, run error handling, notifier error tolerated, repeat
     start/stop.
   - config: defaults, valid, invalid (enabled+0, negative), persistence round-trip,
     old-config-without-fields loads disabled.
   - server: GET/PUT scheduling (auth/CSRF/validation intact), manual still works, enabled config
     starts a scheduler, scheduler runs cleanup, notification failure doesn't fail cleanup,
     config-change restarts scheduler, shutdown during a run is clean.

### Out of scope
Rewriting the Phase 6 retention engine; automatic backup scheduling (this only schedules
cleanup); automatic retry of failed jobs; other notifier targets (email/Slack/webhook) beyond
the slog implementation; changing auth/CSRF/loopback security; new providers; UI redesign.

### Design decisions
- **Interval type**: `int` days (`ScheduledCleanupIntervalDays`, yaml `scheduledCleanupIntervalDays`),
  consistent with `keepLocalDays`/`keepDriveDays`; simplest to validate and display. A `time.Duration`
  string would diverge from existing int-day config.
- **Scheduler package**: `internal/schedule` with `Clock`/`Ticker` interfaces for DI and a
  deterministic fake clock in tests.
- **Replacing on config change**: on `PUT /api/retention` with scheduling changes, the server stops
  the current scheduler and starts a new one reflecting the new config (started only when enabled).
- **Lifecycle**: `main.go` gained graceful shutdown; the scheduler is created/wired after config
  load and stopped on shutdown.
- **Notifier**: `server.notifier`-style seam defaulting to a slog implementation; errors ignored.

### Files to change
- `internal/config/config.go` + `config_test.go` — scheduling fields, defaults, validate, Save/Load.
- `internal/schedule/` (new) — `schedule.go` + `schedule_test.go` (fake clock).
- `internal/server/cleanup.go` — extend `retentionView`/`retentionInput` + GET/PUT handlers with
  scheduling + status; `runScheduledCleanup` wrapper; notifier + status fields.
- `internal/server/server.go` — scheduler field + wiring, notifier field.
- `internal/server/cleanup_test.go` — scheduling/status integration + concurrency tests.
- `internal/server/static/cloud.js` + `templates/cloud-repositories.html` — "Scheduled Cleanup" section.
- `cmd/server/main.go` — graceful shutdown + scheduler lifecycle.
- `PLAN.md` — this document.

### Phase 7 status
- [x] Read PLAN.md + inspect full Phase 2-6 implementation
- [x] Persist scope/plan (this section)
- [x] Scheduling config (model + defaults + validation + Save/Load + tests)
- [x] `internal/schedule` scheduler + unit tests (fake clock)
- [x] Server status model + notifier + API extensions (GET/PUT)
- [x] Scheduler lifecycle wiring + graceful shutdown in main.go
- [x] UI (Scheduled Cleanup section + cloud.js)
- [x] Server-side integration + concurrency + restart tests
- [x] Verification (`gofmt`, `build`, `vet`, `test`, `node --check`, `-race` attempted)
- [x] Update work-state with results/limitations

### Results
- **Config** (`internal/config/config.go`): `RetentionConfig` gains `ScheduledCleanupEnabled bool`
  (yaml `scheduledCleanupEnabled`) + `ScheduledCleanupIntervalDays int` (yaml
  `scheduledCleanupIntervalDays`). Defaults disabled/`0`. `Validate()` refuses negative intervals
  and rejects `enabled` with interval `<= 0`. Persists via existing Save/Load; old configs load
  disabled. Tests: `TestSchedulingDefaultsDisabled`, `TestSchedulingRoundTrip`,
  `TestSchedulingOldConfigWithoutFields`, `TestSchedulingValidate` (10/10 in package pass).
- **Scheduler** (`internal/schedule/schedule.go` + tests): `Clock`/`Ticker` interfaces (wall-clock
  default), `Scheduler{Interval, Run, Clock}` with idempotent `Start()`/`Stop()`, guaranteed
  non-overlapping runs (busy ticks dropped, serialized), error-tolerant loop, `Running()`, and
  `Stop()` that waits for an in-flight run. `Start()` creates the ticker synchronously before
  launching the goroutine (needed for deterministic fake-clock tests). 7/7 unit tests pass.
- **Server** (`internal/server/cleanup_schedule.go` + `cleanup.go` + `server.go`): `cleanupStatus`
  model + `scheduledView` JSON (`enabled`, `intervalDays`, `running`, `lastStart`, `lastFinish`,
  `lastTrigger`, `success`, `nextRun`, `lastResult` embedding the full `retention.Result`).
  `CleanupNotifier` interface + `slogNotifier` default; notification failures are logged, never
  fail cleanup. `executeCleanup(ctx, trigger)` is the single manual/scheduled entry point,
  serialized by the existing `cleanupMu`. `startSchedulerFor` replaces/stops the scheduler on
  config change; `StartCleanupScheduler`/`StopCleanupScheduler` wire startup/shutdown.
  GET `/api/retention` returns `scheduled`; PUT accepts/validates/persists the new fields,
  restarts the scheduler, and still enforces auth + CSRF.
- **Lifecycle** (`cmd/server/main.go`): `signal.NotifyContext` + `http.Server.Shutdown` (10s
  timeout) + `StopCleanupScheduler()` on shutdown; `http.ErrServerClosed` tolerated.
- **UI** (`templates/cloud-repositories.html` + `static/cloud.js`): "Scheduled cleanup" checkbox +
  interval (days) input + status line (running / last run / next run / last-run errors); save and
  manual-run handlers updated; `node --check` clean.
- **Tests**: 10 new server tests in `cleanup_schedule_test.go` (scheduling defaults, enable/disable,
  validation, scheduled run + status, notifier success/failure tolerance, manual trigger, stop-on-
  disable with no resume, manual/scheduled serialization under `cleanupMu`, graceful stop during a
  run, start-from-config). Deterministic via a fake clock + fake ticker; no sleeps.

### Verification
- `gofmt -l .` clean; `go build ./...` OK; `go vet ./...` clean; `go test -count=1 -timeout 180s ./...` all packages pass; `node --check internal/server/static/cloud.js` OK.
- `go test -race` **not runnable here**: requires cgo (`CGO_ENABLED=1`) and no C compiler (`gcc`)
  is installed on this Windows machine. Concurrency safety is covered by the deterministic
  serialization tests mocking blocked runs. Run `-race` once a C toolchain is available.

### Proposed next phase (Phase 8 candidates, for review)
- Webhook/email notifiers behind the `CleanupNotifier` interface; notifier config in `config.yaml`.
- Per-repo scheduling overrides; missed-run catch-up (run immediately if the process was down
  across a boundary).
- Persisted run history (which schedules, budgets) instead of in-memory `cleanupStatus`.
- Add a C toolchain to CI and enable `go test -race` in the workflow.

### Verification plan
`gofmt -l .` clean; `go build ./...`; `go vet ./...`; `go test -count=1 ./...`;
`node --check` on modified JS; `git status`/`git diff` only intended changes;
`go test -race` attempted (document the known cgo limitation).

---

## Phase 8 — Operational Hardening (COMPLETE)

### Scope
Make the Phase 6 retention engine + Phase 7 scheduled cleanup production-ready:
(1) real notification targets (a generic webhook behind the `CleanupNotifier`
abstraction), (2) per-repository retention overrides inherited over the global
policy, (3) bounded, atomic, persisted cleanup history, (4) race-detection CI,
and (5) failure/recovery hardening (panic isolation at the scheduler boundary,
secondary-failure isolation). Explicitly OUT of scope: rewriting the retention
engine or scheduler, automatic backup scheduling, new providers, UI redesign.

### Design decisions (locked)
- **Notifications**: top-level `Config.Notifications` (yaml `notifications`)
  with `Enabled`, `Type` (`webhook`), `WebhookURL`, `WebhookAuthHeader`,
  `WebhookAuthToken`, `WebhookTimeoutSeconds`, `NotifyOnSuccess`,
  `NotifyOnFailure`. Disabled by default (conservative). The engine stays
  transport-agnostic; the server resolves a `CleanupNotifier` per run from
  config (webhook when enabled, else the existing slog notifier). Secrets
  (`WebhookAuthToken`) are: env-expanded on load, never logged, never returned
  by any API (GET reports `webhookAuthConfigured: true` only), and masked in
  the UI. New package `internal/notify` (mirrors `internal/schedule` style):
  builds a JSON payload (trigger, success, timestamps, counts, errors) and POSTs
  it with a timeout, 2xx validation, body-discard (never logged), and safe error
  wrapping that never includes the token. Auth header name is validated to be a
  valid HTTP header field name.
- **Per-repo overrides**: top-level `Config.RepositoryRetentionOverrides`
  (yaml `repositoryRetentionOverrides`), each `{repositoryId, keepLocal,
  keepLocalDays, keepJobs, keepDriveDays, driveRetentionEnabled}` where the
  repository identifier is the immutable **GitHub repository ID** (`githubId`,
  per Phase 2's canonical identity). Counts/ages are `*int` (nil = inherit
  global; explicit 0 = keep-all for that dimension), and
  `driveRetentionEnabled` is `*bool`. Pure resolver `config.MergeRetention
  (global, override)` — a repo without an override gets exactly today's global
  policy. Validation: positive `repositoryId`, no negative dims, and a
  `driveRetentionEnabled=true` override must have a positive `keepDriveDays`
  either in the override or globally (an override can never silently disable a
  safety constraint). The retention engine gains ONE additive seam:
  `Engine.PolicyFor func(repoID string) Policy` (nil = pure global, so all
  existing engine call sites/tests are unchanged). `Run` becomes a no-op when
  the global policy is fully disabled AND no `PolicyFor` is set (identical to
  today); orphan cleanup + record reconciliation stay gated by the global
  policy so overrides can never enable global orphan deletion.
- **Cleanup history**: new package `internal/history` (`Store` +
  `Entry{StartedAt, FinishedAt, Trigger, Success, Inspected, Retained,
  LocalDeleted, DriveDeleted, OrphanDeleted, Skipped, Missing, ErrorCount,
  Errors, NotifyError}`). Bounded by `Retention.CleanupHistoryLimit` (yaml
  `cleanupHistoryLimit`, default 50, `>= 0`, 0 = recording disabled; misses the
  field in old configs → 50). Persisted to `.gitsafe/cleanup-history.json`
  reusing the `state.Store` atomic write pattern (temp file + fsync + rename).
  Missing/empty history → clean start; corrupt/unsupported → load empty, log
  loudly, keep the file until the next successful write (startup never fails).
  Write and notify failures are isolated: cleanup outcome is never altered.
- **Scheduler hardening**: `internal/schedule.Scheduler` recovers panics at the
  fire-goroutine boundary (configurable `Recover func(any)`; server sets it to
  log + mark the run as failed via `executeCleanup`'s wrapper is NOT used —
  instead the scheduler-level recover logs and continues future ticks; the
  server additionally records the panic as a failed status entry). A tick whose
  run panicked never takes the scheduler down and never overlaps the next tick.
- **API/UI**: `GET /api/retention` gains additive `notifications`,
  `overrides`, and `history` blocks (the latter bounded, newest first, e.g.
  10). `PUT /api/retention` accepts and persists the new sections with the
  same validation + auth + CSRF as today. `POST /api/retention/notify-test`
  (auth + CSRF) fires the current notifier with a sample result. History limits
  and manual/scheduled recording wired through the existing `executeCleanup`.
  Settings save (Phase 1 endpoint) is fixed to preserve the retention
  scheduling/history fields it previously truncated.
- **CI**: extend the existing `.github/workflows/go.yml` with an Ubuntu `test`
  job running `go build ./...`, `go vet ./...`, and `go test -race -count=1
  ./...` (Ubuntu ships a C compiler, so race detection actually runs there).
  The existing build job stays untouched.
- **Concurrency**: audit confirms today's order is `cleanupMu → jobMu →
  stateStore-internal`; new code adds no nested locks. History uses its own
  lock for in-memory ops plus a write mutex so slow disk writes never hold
  `cleanupMu`/`statusMu`. Notifications perform network I/O outside every app
  lock. `schedMu` only guards the pointer, never waits under another lock.

### Files to change
- `internal/config/config.go` + `config_test.go` — Notifications config,
  overrides, `CleanupHistoryLimit`, merge resolver, defaults/validate/save.
- `internal/history/` (new) — `history.go` + `history_test.go`.
- `internal/notify/` (new) — `notify.go` (+ payload/wiring) + `notify_test.go`.
- `internal/retention/retention.go` + `retention_test.go` — `PolicyFor` seam.
- `internal/schedule/schedule.go` + `schedule_test.go` — panic recovery.
- `internal/server/cleanup.go`, `cleanup_schedule.go`, `server.go`, `settings.go`
  + tests — new config blocks, notifier resolution, history wiring,
  overrides resolver, notify-test endpoint, settings-preservation fix.
- `internal/server/static/cloud.js` + `templates/cloud-repositories.html` — UI.
- `cmd/server/main.go` — open history store + wire.
- `.github/workflows/go.yml` — race CI job.
- `PLAN.md` — this document.

### Phase 8 status
- [x] Inspect everything (this run)
- [x] PLAN.md scope (this section)
- [x] Config: notifications + overrides + history limit (model/validate/save/load/tests)
- [x] `internal/history` (atomic, bounded, corrupt-safe) + tests
- [x] `internal/notify` webhook + tests (fake HTTP servers)
- [x] Retention engine `PolicyFor` seam + tests (incl. override-combined behavior)
- [x] Scheduler panic recovery + test
- [x] Server: new GET/PUT blocks, notifier resolution, history Recording,
      overrides resolver, notify-test endpoint, settings-preservation fix
- [x] UI: notifications + overrides + history sections
- [x] CI race job
- [x] Security review + tests (secret exposure, webhook URL/header validation,
      repo-id validation, corrupt-history safety)
- [x] Concurrency audit (lock ordering, no locks across network/disk I/O)
- [x] Verification (`gofmt`, `build`, `vet`, `test`, `node --check`, `-race`)
- [x] Final diff review + PLAN.md COMPLETE + report

---

## Phase 9 — Production Hardening (COMPLETE)

### Scope
Harden the Phase 6/7/8 retention/cleanup + Phase 3 backup machinery for
production use: (1) outbound webhook SSRF guards + secret (token) hygiene,
(2) strict input validation (request bodies, batch sizes, retention override
bounds, history caps), (3) filesystem safety for recorded bundle paths and
Drive file IDs (hard ownership check before destructive deletes), (4) atomic
config persistence, (5) idempotency/recovery + lock-ordering audit, (6) global
concurrency bounds on protected backups, (7) bounded network timeouts, and
(8) CI lint/race jobs. Explicitly OUT of scope: UI redesign, new providers,
rewriting the retention engine or scheduler.

### Design decisions (locked)
- **SSRF**: webhook URLs validated with a guard set — default ports allowed
  (loopback/private unicast are permitted; the API is loopback-bound), blocked
  classes: unspecified, multicast, link-local (IPv4, IPv6, and mapped), and
  cloud-metadata; userinfo rejected; hostname required; URL length ≤ 2048;
  redirects capped at 5 with per-hop re-validation. DNS-rebinding TOCTOU is
  documented and deferred. Zero-configuration defaults remain unchanged.
- **Secrets**: `WebhookAuthToken` is NOT env-expanded at load (Save would
  persist the resolved secret); it is expanded only at webhook delivery time
  in `internal/notify.resolve()`. Errors/URLs are redacted to `scheme://host`.
  No API or template ever returns the token (GET reports only
  `webhookAuthConfigured: bool`). Config `Save` never persists resolved
  secrets.
- **Input validation**: every JSON body flows through `decodeJSONBody`
  (1 MiB cap, unknown-field rejection). Batch caps: 200 protected-repo
  protects, 500 per protect batch. Retention: ≤ 1000 overrides per PUT (422),
  per-override validation (positive repository id, non-negative knobs),
  `CleanupHistoryLimit ≤ 10000` (422 via `policy.Validate`), history preview
  capped at 10.
- **Filesystem safety**: recorded bundle paths are deleted only when
  `validRecordedName` holds (plain basename + `recordedBundlePattern`, legacy
  and v2 names); anything else (traversals `../`, absolute paths, drive-rooted
  names) is skipped and surfaced in run errors, never deleted. Drive file IDs
  must match `^[A-Za-z0-9_-]{10,100}$` before any delete, enforced both in the
  retention engine and in `cloud.DeleteFile` (defense in depth).
- **Atomic persistence**: `Config.Save` writes via unique temp file in the
  same directory + fsync + mode preservation + rename, serialized by a
  package-level mutex (Windows cross-rename races). The state store already
  followed this pattern; crash-recovery tests exist.
- **Concurrency/resource limits**: lock ordering is acyclic (`cleanupMu →
  jobMu → stateStore-internal`). A global `backupSem` (capacity 4) caps
  concurrently running protected mirrors; per-repo in-flight (409) check
  unchanged. Network I/O (notify/OAuth/GitHub/Drive) happens outside app
  locks; Drive uploads/deletes bounded by 30 min / 2 min context deadlines.
- **CI**: `.github/workflows/go.yml` gains a `lint` job (gofmt + vet) and a
  `test` job (`go test` + `go test -race` on Ubuntu where cgo exists).

### Files changed
- `internal/notify/notify.go` + tests — SSRF guards, redirect cap, URL length
  limit, delivery-time token expansion, error/URL redaction, 9 security tests.
- `internal/server/server.go`, `cleanup.go`, `settings.go`, `setup.go`,
  `backupjobs.go` + tests — `decodeJSONBody` (1 MiB) at all JSON sites, batch
  caps, retention override/history bounds + validation, `backupSem`,
  `sanitizeMessage` guard, orphan-job recovery plans.
- `internal/config/config.go` + tests — atomic `Save`, `configSaveMu`,
  `maxCleanupHistoryLimit`, concurrency-safety tests.
- `internal/retention/retention.go` + tests — `validRecordedName`,
  `recordedBundlePattern`, `validDriveFileID`; skip/probe guards before any
  destructive delete + traversal/the missing-bundle/reconcile tests.
- `internal/cloud/drive.go` — `driveFileIDValid` refusal in `DeleteFile`,
  upload/delete timeouts.
- `go.mod`/`go.sum` — `github.com/zalando/go-keyring` (+ `wincred`/`dbus`
  indirects) now declared for the keyring token store.
- `.github/workflows/go.yml` — `build`, `lint`, `test` (incl. `-race`) jobs.

### Verification
`gofmt -l .` clean; `go build ./...`; `go vet ./...`; full
`go test -count=1 -timeout 180s ./...` green across all 16 packages
(server 32.7s, scanner 22.7s, backup 13.9s, notify/config ~7-13s).
`go test -race` is attempted but the local toolchain has no cgo (`-race`
requires cgo; `CGO_ENABLED=1` unavailable here); the added CI `test` job runs
the race suite on Ubuntu. Known environment quirk: `go mod tidy`/`go test`
orchestration intermittently hangs in the local Windows shell; direct
binary/test invocations and output-redirected runs complete reliably.

---

## Proposed next phase (Phase 10 candidates, for review)
- **Automatic backup scheduling** (cron-expression or interval-based jobs with
  jittered starts to cadence with Drive usage quotas) + missed-run catch-up
  for backup jobs, mirroring the Phase 7 schedule engine.
- **Backup goal/target quotas on Drive** (soft cap with warnings, hard cap
  refusal) enforced at mirror time, not only at retention time.
- **Remote webhook signature verification** (HMAC request signing so
  recipients can authenticate GitSafe-originated payloads).
- **Backup-verification retry with exponential backoff** for transient Drive
  upload failures, plus a `/api/backup-jobs` batch endpoint for the UI.
- **End-to-end contract tests** against a throwaway GitHub + Drive sandbox in
  CI (marked skippable without credentials).
