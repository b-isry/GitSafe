# Contributing to GitSafe

Thanks for helping! GitSafe is a small Go web app: GitHub OAuth sign-in, and
backups of each repository as a `git bundle` to Google Drive or as a local
download. This guide gets you from clone to a passing pull request.

- [Prerequisites](#prerequisites)
- [Quick start](#quick-start)
- [Create the OAuth apps](#create-the-oauth-apps)
- [Environment variables](#environment-variables)
- [Make targets](#make-targets)
- [Running PostgreSQL locally](#running-postgresql-locally)
- [Windows](#windows)
- [Commits and pull requests](#commits-and-pull-requests)

Looking for something to work on? See [docs/good-first-issues.md](docs/good-first-issues.md).
Security problems: please **don't** open an issue; see [SECURITY.md](SECURITY.md).

## Prerequisites

- Go (the version in `go.mod`; `go` will fetch a newer toolchain automatically if needed)
- `git` (GitSafe shells out to it to create bundles)
- GNU make (optional; every target is a plain `go` command, see [Make targets](#make-targets))
- Docker (optional, for local PostgreSQL / `make docker`)
- [golangci-lint](https://golangci-lint.run/welcome/install/) v2 (optional; CI runs it)

## Quick start

```bash
git clone https://github.com/b-isry/gitsafe.git
cd gitsafe
cp .env.example .env        # then fill in the GitHub OAuth values (below)
make test                   # unit tests need no credentials or network
make run                    # http://127.0.0.1:8080
```

**Use `http://127.0.0.1:8080`, not your LAN IP.** With a loopback
`GITSAFE_BASE_URL` (the default), GitSafe runs as a local install and only
accepts requests from `127.0.0.1`.

### What you need for local development

| Want to... | You need |
|------------|----------|
| Run the unit tests / linters | nothing (all external services are stubbed) |
| Sign in and back up repositories locally | a **GitHub OAuth App** (free, 2 minutes) |
| Back up to Google Drive | additionally a **Google OAuth client** |
| Test PostgreSQL persistence | Docker (`make db-up`) or any PostgreSQL |

**There is no "fake login" mode, on purpose.** Every account action in
GitSafe acts on a real GitHub token, and a sign-in bypass is exactly the sort
of switch that ends up enabled in production by accident. Creating a personal
GitHub OAuth App pointed at `127.0.0.1` is quick and safe, so that's the
supported route.

**Google Drive is optional locally.** Leave `GITSAFE_DRIVE_CLIENT_ID` and
`GITSAFE_DRIVE_CLIENT_SECRET` empty. The Drive pill then shows "Not enabled",
and you can choose the **Local** backup destination in the dashboard: the
bundle is built on the server and downloaded by your browser. This exercises
the whole backup pipeline (GitHub API, clone, bundle, job tracking) without
Google.

Without `DATABASE_URL`, state goes to `.gitsafe/` JSON files. Tokens go to the
OS keyring, or, if `GITSAFE_TOKEN_KEY` is set, to an encrypted file in
`GITSAFE_DATA_DIR` (default `.gitsafe/`). On a headless Linux box without a keyring service, set
`GITSAFE_TOKEN_KEY` to any throwaway value.

## Create the OAuth apps

The callback URLs are built from `GITSAFE_BASE_URL` (default
`http://127.0.0.1:8080`):

| Provider | Redirect / callback URL |
|----------|-------------------------|
| GitHub | `http://127.0.0.1:8080/api/auth/github/callback` |
| Google | `http://127.0.0.1:8080/api/auth/drive/callback` |

### GitHub OAuth App (required)

1. Go to <https://github.com/settings/applications/new> (Settings → Developer
   settings → OAuth Apps → New OAuth App).
2. Set **Homepage URL** to `http://127.0.0.1:8080` and **Authorization callback
   URL** to `http://127.0.0.1:8080/api/auth/github/callback`.
3. Register it, then click **Generate a new client secret**.
4. Put the values in `.env` as `GITSAFE_GITHUB_CLIENT_ID` and
   `GITSAFE_GITHUB_CLIENT_SECRET`.

GitSafe requests the `repo` scope. OAuth Apps have no read-only scope for
private repositories, so `repo` is the minimum; GitSafe only reads and
clones.

### Google OAuth client (optional, for Drive)

1. Create or pick a project at <https://console.cloud.google.com/> and enable
   the **Google Drive API** (APIs & Services → Library).
2. Configure the OAuth consent screen (Google Auth Platform):
   - User type: **External**.
   - Add yourself under **Test users**. Testing mode is fine for development,
     but refresh tokens then expire after 7 days.
   - Scope: only `https://www.googleapis.com/auth/drive.file`.
3. Go to Credentials → Create credentials → OAuth client ID → **Web
   application**.
4. Add the authorized redirect URI
   `http://127.0.0.1:8080/api/auth/drive/callback`.
5. Put the values in `.env` as `GITSAFE_DRIVE_CLIENT_ID` and
   `GITSAFE_DRIVE_CLIENT_SECRET`.

Don't reuse production OAuth apps or secrets for development.

## Environment variables

All variables are listed in [`.env.example`](.env.example). `.env` is loaded
automatically at startup and is gitignored. Never commit it.

| Variable | Local dev | Purpose |
|----------|-----------|---------|
| `GITSAFE_BASE_URL` | optional (`http://127.0.0.1:8080`) | Public URL; OAuth callbacks are built from it. A non-loopback value switches on production checks. |
| `GITSAFE_GITHUB_CLIENT_ID` / `GITSAFE_GITHUB_CLIENT_SECRET` | needed to sign in | GitHub OAuth App |
| `GITSAFE_DRIVE_CLIENT_ID` / `GITSAFE_DRIVE_CLIENT_SECRET` | optional | Google OAuth client (Drive backups) |
| `DATABASE_URL` | optional | PostgreSQL. Required in production. |
| `GITSAFE_TOKEN_KEY` | required with `DATABASE_URL` | Key for encrypting stored OAuth tokens. Locally, any throwaway value. |
| `GITSAFE_DATA_DIR` | optional | Directory for the file-backed token store |
| `GITSAFE_SESSION_KEY` | optional | Cookie-signing key. Defaults to one derived from `GITSAFE_TOKEN_KEY`, or a random key per process. |
| `GITSAFE_CRON_SECRET` | optional | Enables `POST /api/cron/backup` (daily backups), at least 32 characters |
| `GITSAFE_TRUSTED_PROXY_HEADER` | optional | Header for client IP rate limiting (e.g. `CF-Connecting-IP`, `X-Real-IP`, or `X-Forwarded-For`) |
| `PORT` | optional (8080) | Listen port |

Generate random values with `openssl rand -hex 32`.

## Make targets

| Target | Equivalent command |
|--------|--------------------|
| `make run` | `go run ./cmd/server` |
| `make build` | `go build -o bin/gitsafe ./cmd/server` |
| `make test` | `go test -count=1 ./...` |
| `make race` | `go test -count=1 -race ./...` |
| `make lint` | `gofmt -l .` (must print nothing), `go vet ./...`, `go vet -tags=postgres ./...`, `golangci-lint run ./...` if installed |
| `make fmt` | `gofmt -w .` |
| `make docker` | `docker build -t gitsafe:dev .` |
| `make db-up` / `make db-down` | `docker compose up -d db` / `docker compose down` |
| `make test-postgres` | `GITSAFE_TEST_DATABASE_URL=postgresql://gitsafe:gitsafe@127.0.0.1:5432/gitsafe?sslmode=disable go test -tags=postgres -count=1 -p 1 ./...` |

Run `make help` for the list. CI (`.github/workflows/go.yml`) runs `lint`,
`build`, `test`, `race` and the PostgreSQL tests on every pull request.

## Running PostgreSQL locally

```bash
make db-up
# in .env:
#   DATABASE_URL=postgresql://gitsafe:gitsafe@127.0.0.1:5432/gitsafe?sslmode=disable
#   GITSAFE_TOKEN_KEY=<any throwaway value>
make run
make test-postgres
```

The tables are created automatically at startup.

To run the app in a container as well, use
`docker compose --profile app up --build`. This works on **Linux only**: the
app uses host networking, because a local install only accepts connections
from 127.0.0.1 and Docker's port publishing would make requests arrive from
the bridge network. On macOS or Windows, run PostgreSQL in Docker and the app
with `make run` / `go run ./cmd/server`.

## Windows

`make` is not needed. Use the commands in the [Make targets](#make-targets)
table directly in PowerShell, for example:

```powershell
go test -count=1 ./...
go test -count=1 -race ./...   # needs cgo (a C toolchain such as MinGW) for -race
gofmt -l .
go vet ./...
golangci-lint run ./...
docker compose up -d db
$env:GITSAFE_TEST_DATABASE_URL = "postgresql://gitsafe:gitsafe@127.0.0.1:5432/gitsafe?sslmode=disable"
go test -tags=postgres -count=1 -p 1 ./...
```

Alternatively, use WSL2, where the Linux instructions apply unchanged.
Install GNU make with `winget install GnuWin32.Make`, `choco install make` or
`scoop install make` if you prefer the targets.

## Commits and pull requests

- Branch from `main` and keep each PR focused on one change.
- Use [Conventional Commits](https://www.conventionalcommits.org/) for commit
  messages and PR titles:
  `fix(auth): …`, `feat: …`, `docs: …`, `ci: …`, `build: …`, `chore: …`,
  `test: …`, `refactor: …`. Explain *why* in the body.
- Add or update tests for behavior changes. The test suite must not need
  network access or real credentials; use the existing function-field seams
  (`connectGitHub`, `driveUpload`, `bundleBackup`, `githubLister`, …).
- Before pushing, run `make lint test race`. Run `make test-postgres` if you
  touched storage.
- Update `README.md`, `.env.example` and `render.yaml` when you add
  configuration.
- Never log or display tokens, secrets or OAuth codes. Never commit `.env` or
  `credentials.json`.
- Fill in the pull request template. A maintainer reviews; CI must be green.

By contributing you agree that your contribution is licensed under the
project's [MIT License](LICENSE).
