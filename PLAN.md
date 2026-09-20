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

## Phase 6 & 7 — Retention, Cleanup, Scheduling & Notifications (REMOVED)

The retention/cleanup system (local + Drive retention policy, manual cleanup
trigger, orphan/job-history cleanup), the scheduled-cleanup orchestrator, and
the webhook notification feature were removed in the v2 simplification: GitSafe
is intentionally a simple pipeline (GitHub → protected repo → bundle → Drive
upload) with no automatic deletion and no outbound notifications. The deleted
code lived in `internal/retention`, `internal/schedule`, `internal/notify`,
`internal/history`, `internal/server/cleanup*.go`, and the `config` retention /
notification / per-repository-override fields.

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

> Note: the notifier, per-repo override, cleanup-history, and scheduler-panic
> components described in this phase were built on the retention system and were
> removed with it (see Phase 6 & 7 note). Code preserved from this phase:
> `decodeJSONBody` request guarding, batch caps, `backupSem`, atomic
> `Config.Save`, the keyring token store, and the CI build/lint/test jobs.

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

> Note: the SSRF/secret-hygiene guards, filesystem-safety checks, and Drive
> file-ID validation targeted the removed webhook and retention/cleanup code.
> Code preserved from this phase: `decodeJSONBody`, atomic `Config.Save`,
> `backupSem`, `sanitizeMessage`, the keyring token store, and the CI
> lint/race jobs.

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
