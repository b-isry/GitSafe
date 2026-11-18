# Candidate issues for contributors

These are known gaps from the October 2026 bug sweep that were deliberately
left unfixed. Each entry is written so it can be turned into a GitHub issue
directly. Difficulty: 🟢 good first issue · 🟡 medium · 🔴 needs design.
Please comment on or open the issue before starting, so work isn't
duplicated.

---

## 🟢 Add logout and "delete account" buttons to the dashboard

- **Where:** `internal/server/templates/layout.html`,
  `internal/server/static/cloud.js`
- **Problem:** The APIs exist (`POST /api/auth/logout` and
  `POST /api/account/delete`, both need the `X-CSRF-Token` header), but the UI
  has no way to call them.
- **Done when:**
  - A "Sign out" action calls logout and reloads the page.
  - "Delete account" asks for confirmation (for example, typing the GitHub
    login) and then calls the delete endpoint.
- **Tests:** The handler tests already exist. Check the UI by hand and
  describe what you checked in the PR.

## 🟢 Distinguish GitHub 403 causes instead of always asking to reconnect

- **Where:** `internal/providers/github.go`
- **Problem:** Every HTTP 403 is mapped to `ErrUnauthorized`, so SAML-SSO
  enforcement, an org blocking OAuth apps, or a secondary rate limit all show
  "reconnect GitHub", which doesn't help.
- **Done when:**
  - A 403 with `X-RateLimit-Remaining: 0` or a "secondary rate limit"
    message maps to `ErrRateLimited`.
  - A SAML 403 (`X-GitHub-SSO` header) gets its own error with a helpful
    message.
- **Tests:** Table-driven tests with `httptest` responses.

## 🟢 Add the repository owner to Drive bundle names

- **Where:** `internal/archiver`, `internal/server/backupjobs.go`
- **Problem:** Bundle names leave out the owner, so `alice/app` and `acme/app`
  produce files that look the same in Drive. Each repository still keeps its
  own Drive file ID, so nothing is overwritten.
- **Done when:** New bundles are named `<owner>__<repo>.bundle` (or similar).
  Existing Drive files keep working because they are updated by file ID.
  Consider renaming the Drive file on its next update.
- **Note:** This changes what users see in Drive. Mention it in the PR.

## 🟡 Use the real client IP behind Cloudflare and Render for rate limiting

- **Where:** `internal/server/ratelimit.go` (`clientIP`)
- **Problem:** The leftmost `X-Forwarded-For` entry is trusted, and any client
  can set it, so per-IP limits can be bypassed.
- **Done when:**
  - The source is configurable, e.g. `GITSAFE_TRUSTED_PROXY_HEADER=CF-Connecting-IP`,
    or "rightmost untrusted hop" with a trusted hop count.
  - The default is safe: `RemoteAddr`, or the rightmost `X-Forwarded-For`
    entry added by Render.
  - The choice is documented.
- **Tests:** Spoofed-header cases in `ratelimit_test.go`.

## 🟡 Add timeouts to PostgreSQL store calls

- **Where:** `internal/state/postgres_store.go`,
  `internal/tokenstore/postgres_store.go`
- **Problem:** Queries use `context.Background()` with no deadline, so a hung
  database connection blocks the request indefinitely.
- **Done when:** Every query runs with a bounded context (for example 10 s,
  like `internal/server/daily_store.go`). Ideally `Get`/`Set`/`Save` accept a
  `context.Context` from the request, which means changing the
  `StateStore`/`TokenStore` interfaces and their callers.
- **Tests:** Run with `make test-postgres`.

## 🟡 Limit Drive revision growth on re-backups

- **Where:** `internal/cloud/drive.go`
- **Problem:** Re-backups update the Drive file in place, which keeps old
  revisions. Drive prunes them eventually, but they count toward the user's
  quota in the meantime.
- **Done when:** Decide and document a policy. For example, upload the new
  content with `keepRevisionForever=false` and delete older revisions beyond
  N, or skip uploading when the bundle SHA-256 is unchanged from the last
  record (the hash is already stored).
- **Tests:** Extend the fake Drive server in `internal/cloud/drive_test.go`.

## 🔴 Server-side session revocation

- **Where:** `internal/server/session.go`
- **Problem:** Sessions are stateless signed cookies (so sign-in survives
  restarts on the free tier). Logout only deletes the cookie, so a stolen
  cookie stays valid until it expires (7 days).
- **Options:**
  - A small `sessions` table, or a per-user "valid after" timestamp in
    PostgreSQL that is checked on sensitive endpoints.
  - Shorter sliding expiry.
- **Constraint:** It must keep working across restarts and stay cheap on a
  free database that suspends when idle.

## 🔴 Narrower GitHub permissions through a GitHub App

- **Where:** `internal/githuboauth`, `internal/providers`
- **Problem:** OAuth Apps have no read-only scope for private repositories, so
  GitSafe must request `repo`, which includes write access.
- **Option:** A GitHub App with "Contents: read-only" and "Metadata:
  read-only", using user-to-server tokens (which expire and refresh) and
  installation selection.
- **Constraint:** Plan the migration of existing users.
