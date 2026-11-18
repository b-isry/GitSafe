# GitSafe

[![CI](https://github.com/b-isry/gitsafe/actions/workflows/go.yml/badge.svg?branch=main)](https://github.com/b-isry/gitsafe/actions/workflows/go.yml)
[![Daily backup](https://github.com/b-isry/gitsafe/actions/workflows/daily-backup.yml/badge.svg)](https://github.com/b-isry/gitsafe/actions/workflows/daily-backup.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/b-isry/gitsafe)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

GitSafe backs up your GitHub repositories. Sign in with GitHub and GitSafe
saves a complete, restorable `git bundle` of each repository to a `GitSafe`
folder in your own Google Drive (or downloads it to your computer).

## Quick Start for Contributors

To run and test the GitSafe web dashboard locally, you will need a personal GitHub OAuth App:

1. Register an OAuth App on GitHub at https://github.com/settings/applications/new
   - Application name: `GitSafe Local Dev`
   - Homepage URL: `http://127.0.0.1:8080`
   - Authorization callback URL: `http://127.0.0.1:8080/api/auth/github/callback`
2. Copy `.env.example` to `.env` and fill in:
   ```bash
   cp .env.example .env
   ```
   ```env
   GITSAFE_GITHUB_CLIENT_ID=your_client_id
   GITSAFE_GITHUB_CLIENT_SECRET=your_client_secret
   ```
3. Run the server:
   ```bash
   go run ./cmd/server
   ```
4. Visit `http://127.0.0.1:8080` in your browser.

There is intentionally no mock login mode. GitSafe acts directly on real GitHub tokens to clone repositories, and omitting a fake sign-in path ensures mock credentials cannot accidentally be deployed to production. Google Drive is optional for local development: you can choose the Local destination to test the backup pipeline without Google credentials.

## Features

- Sign in with GitHub; list all repositories you can access
- One-click or "Back up all" backups to Google Drive or a local download
- Re-backups replace the previous Drive file in place (Drive keeps revisions)
- Opt-in **daily backups** to Drive that run while you are signed out
- Live progress for uploads, backup history per repository

## Setup

1. Clone the repository:
```bash
git clone https://github.com/b-isry/gitsafe.git
cd gitsafe
```

2. Install dependencies:
```bash
go mod download
```

3. Configure the application base URL (optional for local dev, **required for production**):
   - Set `GITSAFE_BASE_URL` to the public URL where GitSafe will be reached.
   - Local development (default if unset): `http://127.0.0.1:8080`
   - Production (e.g. cloud deployment): `https://gitsafe.example.com`
   - The URL must include the scheme (http/https) and must not have a trailing slash.
   - GitSafe constructs both the GitHub and Google Drive OAuth callback URLs from this base URL:
     - GitHub callback: `<BASE_URL>/api/auth/github/callback`
     - Drive callback: `<BASE_URL>/api/auth/drive/callback`

4. Configure GitHub OAuth (required for the Cloud Repositories feature):
   - Register a GitHub OAuth App at https://github.com/settings/applications/new
     - Homepage URL: your `GITSAFE_BASE_URL` (e.g. `https://gitsafe.example.com`)
     - Authorization callback URL: `<GITSAFE_BASE_URL>/api/auth/github/callback`
   - Copy `.env.example` to `.env` and fill in the values:
   ```bash
   cp .env.example .env
   ```
   ```
   GITSAFE_GITHUB_CLIENT_ID=your_client_id
   GITSAFE_GITHUB_CLIENT_SECRET=your_client_secret
   ```
   - `.env` is loaded automatically at startup (and is gitignored). The client
     id can alternatively be set in `config.yaml` under `github.clientId`; the
     environment variable takes precedence when both are set. The client secret
     is always read from the environment and is never shown in the UI.

5. Configure Google Drive OAuth (required to back repositories up to Google
   Drive):
   - Create a Google Cloud OAuth 2.0 **Web application** client:
     - Go to https://console.cloud.google.com/apis/credentials
     - Click **Create Credentials** → **OAuth client ID**, choose
       **Web application**.
     - Under **Authorized redirect URIs**, add exactly:
       `<GITSAFE_BASE_URL>/api/auth/drive/callback`
     - Copy the **Client ID** and **Client Secret**.
   - Add them to `.env`:
     ```
     GITSAFE_DRIVE_CLIENT_ID=your_client_id
     GITSAFE_DRIVE_CLIENT_SECRET=your_client_secret
     ```
   - Configure the **OAuth consent screen** (APIs & Services → OAuth consent
     screen / "Google Auth Platform"):
     - User type **External**; app name, support email, and developer email.
     - App domain links: home page `<GITSAFE_BASE_URL>/`, privacy policy
       `<GITSAFE_BASE_URL>/privacy`, terms `<GITSAFE_BASE_URL>/terms`.
     - Scopes (Data access): add only
       `https://www.googleapis.com/auth/drive.file` (plus the default
       `openid`/`email` if listed). `drive.file` is a non-sensitive scope, so
       Google verification is not required for it.
     - Enable the **Google Drive API** for the project.
     - **Publish the app ("In production")**. While the app is in *Testing*,
       Google expires refresh tokens after 7 days, which would silently break
       daily backups every week.
   - GitSafe requests only the `drive.file` scope, so it sees, edits, and
     deletes only the files and folders it creates in your Drive, never your
     other Drive data. The client id may alternatively be set in `config.yaml`
     under `driveOAuth.clientId`; the environment variable takes precedence.
     The client secret is always read from the environment and is never shown
     in the UI.

## Security and Scopes

GitSafe requests minimal permissions necessary to perform disaster recovery backups:

- **GitHub (`repo`)**: GitHub OAuth Apps do not provide a read-only scope for private repositories. The `repo` scope is the minimum permission that allows GitSafe to clone private repositories. GitSafe only reads repository metadata and executes mirror clones; it never writes, modifies, pushes, or deletes any code or settings on GitHub.
- **Google Drive (`https://www.googleapis.com/auth/drive.file`)**: GitSafe uses the restricted, per-file `drive.file` scope. It can only see, create, and update files and folders that GitSafe itself creates. It has zero access to your existing Google Drive files, documents, or photos.

## Usage

```bash
go run ./cmd/server
```

Then open your `GITSAFE_BASE_URL` (or `http://127.0.0.1:8080` locally) and click **Connect GitHub** on the Cloud
Repositories page to authorize through GitHub's OAuth flow, then **Connect
Google Drive** (from the Cloud Repositories page). Backups are
uploaded to the connected account's Google Drive.

## Configuration

- `config.yaml`: server settings (days, outputPath, cloud, github, driveOAuth, baseUrl)
- `.env`: deployment credentials (`GITSAFE_BASE_URL`, `GITSAFE_GITHUB_CLIENT_ID`,
  `GITSAFE_GITHUB_CLIENT_SECRET`, `GITSAFE_DRIVE_CLIENT_ID`,
  `GITSAFE_DRIVE_CLIENT_SECRET`)
- `DATABASE_URL`: optional PostgreSQL connection string. When set, per-user
  tokens and state use PostgreSQL. Production requires it; local development
  falls back to file-backed state and encrypted file or OS-keyring tokens.
- `GITSAFE_TOKEN_KEY`: required with `DATABASE_URL` and in production. It
  derives the AES-256 key used to encrypt database-backed tokens.
- `GITSAFE_DATA_DIR`: optional local directory for the file-backed token fallback.
- `GITSAFE_SESSION_KEY`: optional key for signing the session and OAuth-state
  cookies. Defaults to a key derived from `GITSAFE_TOKEN_KEY`, so sign-ins
  survive restarts and redeploys. Changing either signs everyone out.
- `GITSAFE_CRON_SECRET`: optional (≥ 32 characters). Enables opt-in daily
  backups through `POST /api/cron/backup`; see [Daily backups](#daily-backups).
- `GITSAFE_TRUSTED_PROXY_HEADER`: optional header for client IP rate limiting (e.g. `CF-Connecting-IP`, `X-Real-IP`, or `X-Forwarded-For`). Defaults to `RemoteAddr`.
- `PORT`: port to bind the HTTP server (default: 8080). On Render this is set automatically.

The required PostgreSQL tables are created with `CREATE TABLE IF NOT EXISTS`
at server startup. A failed connection or startup round-trip prevents the
server from starting.

## Render Deployment

On Render, set the following environment variables:

| Variable | Description | Example |
|----------|-------------|---------|
| `GITSAFE_BASE_URL` | **Required.** The public URL of the deployed service. | `https://gitsafe.example.com` |
| `GITSAFE_GITHUB_CLIENT_ID` | GitHub OAuth App Client ID | `...` |
| `GITSAFE_GITHUB_CLIENT_SECRET` | GitHub OAuth App Client Secret | `...` |
| `GITSAFE_DRIVE_CLIENT_ID` | Google Drive OAuth Client ID | `...` |
| `GITSAFE_DRIVE_CLIENT_SECRET` | Google Drive OAuth Client Secret | `...` |
| `GITSAFE_TOKEN_KEY` | **Required.** AES encryption key for OAuth tokens. | `...` |
| `DATABASE_URL` | **Required.** External PostgreSQL connection string. | `postgresql://...` |
| `GITSAFE_SESSION_KEY` | Optional. Cookie-signing key (defaults to one derived from `GITSAFE_TOKEN_KEY`). | `openssl rand -hex 32` |
| `GITSAFE_CRON_SECRET` | Optional. Enables daily backups; must match the GitHub Actions secret. | `openssl rand -hex 32` |
| `GITSAFE_TRUSTED_PROXY_HEADER` | Optional. Trusted header for client IP rate limiting. | `CF-Connecting-IP` |

Register the GitHub and Google OAuth redirect URIs using your `GITSAFE_BASE_URL`:
- GitHub: `https://gitsafe.example.com/api/auth/github/callback`
- Google Drive: `https://gitsafe.example.com/api/auth/drive/callback`

The `PORT` variable is automatically provided by Render and does not need to be set manually.

Render's health check uses `GET /healthz` (it does not touch the database, so
a suspended free Postgres is not woken by health checks).

Sign-in state is kept in signed cookies, not server memory, so a free
instance restarting or redeploying while someone is on GitHub's consent
screen no longer fails with "session expired; start over". Always open the
app on the `GITSAFE_BASE_URL` host; sign-in started elsewhere is redirected
there first.

## Daily backups

Users can turn on **Daily backups** in the dashboard. Once a day GitSafe then
backs up every repository of theirs that changed since its last Drive backup,
while they are signed out. It costs nothing extra: a scheduled GitHub Actions
workflow wakes the free Render instance and triggers the run.

How it works:

1. `.github/workflows/daily-backup.yml` runs daily (01:17 UTC) or on demand
   (Actions → Daily backup → Run workflow). It wakes the service through
   `/healthz` (retrying through the cold start), then calls
   `POST /api/cron/backup` with `Authorization: Bearer <GITSAFE_CRON_SECRET>`.
2. The server answers `202 Accepted` and works in the background: users one
   after another, each user's repositories one at a time, skipping
   repositories with a fresh Drive backup (< 20 h) or no pushes since the
   last one. A run is capped at 50 minutes; whatever is left is picked up by
   the next run.
3. The workflow polls `GET /api/cron/backup` (same secret) every minute until
   the run finishes, which also keeps the free instance awake. If the instance
   restarted mid-run, the workflow starts it again (it resumes by skipping
   finished repositories).
4. Each user's result (succeeded / partly / failed, counts, first failures)
   is shown under **Daily backups** in the dashboard. If GitHub or Google
   access was revoked or expired, the user is marked, skipped, and shown a
   **Reconnect** button; reconnecting clears it. Disconnecting GitHub or
   Drive turns daily backups off; deleting the account removes the setting.

The GitHub token and the Google refresh token (`access_type=offline`,
`prompt=consent`) are stored AES-GCM encrypted in PostgreSQL (`user_tokens`,
key from `GITSAFE_TOKEN_KEY`); the opt-in and last-run status live in the
`daily_backup_settings` table, created automatically at startup.

Setup:

1. Generate a secret: `openssl rand -hex 32`.
2. Render → your service → Environment: add `GITSAFE_CRON_SECRET` with that
   value and redeploy.
3. GitHub → this repository → Settings → Secrets and variables → Actions:
   - Add a repository variable `GITSAFE_URL` (or secret `GITSAFE_APP_URL`) = `https://gitsafe.example.com` (your
     `GITSAFE_BASE_URL`, no trailing slash)
   - Add a repository secret `GITSAFE_CRON_SECRET` = the same secret
4. Make sure the Google OAuth app is **published (In production)**, otherwise
   Google refresh tokens expire after 7 days.
5. Actions tab → **Daily backup** → **Run workflow** once to check it.

Scheduled workflows run only from the default branch and may start late.
GitHub disables them in public repositories after 60 days without commits
(re-enable from the Actions tab). In a private repository the workflow's run
time counts against the free 2,000 Actions minutes per month.

## Testing

PostgreSQL integration tests require a running PostgreSQL database. They are
excluded from the default test run with the `postgres` build tag:

```bash
GITSAFE_TEST_DATABASE_URL='postgresql://postgres:postgres@127.0.0.1:5432/gitsafe_test?sslmode=disable' \
  go test -tags=postgres -count=1 ./...
```

The test database must allow the test user to create and drop rows in the
`user_tokens`, `user_state` and `daily_backup_settings` tables.

## Contributing

Contributions are welcome. Start with **[CONTRIBUTING.md](CONTRIBUTING.md)**,
which covers local setup, creating the GitHub and Google OAuth apps for
`127.0.0.1`, environment variables, and the commit and PR conventions.

```bash
cp .env.example .env    # add a GitHub OAuth App (Drive is optional locally)
make test               # no credentials needed
make lint race
make run                # http://127.0.0.1:8080
make db-up              # optional local PostgreSQL (docker compose)
```

- Ideas to pick up: [docs/good-first-issues.md](docs/good-first-issues.md)
- Bugs and feature requests: [open an issue](https://github.com/b-isry/gitsafe/issues/new/choose)
- Security issues: report privately, see [SECURITY.md](SECURITY.md)

## License

This project is licensed under the MIT License - see the LICENSE file for details.