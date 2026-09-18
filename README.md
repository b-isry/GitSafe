# GitSafe

GitSafe is a tool that helps you manage and backup your Git repositories. It identifies stale repositories and creates backups of them in Google Drive.

## Features

- Scan directories for Git repositories
- Identify repositories that haven't been updated in a specified time period
- Create zip archives of repositories
- Upload repository backups to Google Drive
- Progress tracking during uploads

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
   - Production (e.g., Render): `https://gitsafe.onrender.com`
   - The URL must include the scheme (http/https) and must not have a trailing slash.
   - GitSafe constructs both the GitHub and Google Drive OAuth callback URLs from this base URL:
     - GitHub callback: `<BASE_URL>/api/auth/github/callback`
     - Drive callback: `<BASE_URL>/api/auth/drive/callback`

4. Configure GitHub OAuth (required for the Cloud Repositories feature):
   - Register a GitHub OAuth App at https://github.com/settings/applications/new
     - Homepage URL: your `GITSAFE_BASE_URL` (e.g. `https://gitsafe.onrender.com`)
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
   - GitSafe requests only the `drive.file` scope, so it sees, edits, and
     deletes only the files and folders it creates in your Drive — never your
     other Drive data. The client id may alternatively be set in `config.yaml`
     under `driveOAuth.clientId`; the environment variable takes precedence.
     The client secret is always read from the environment and is never shown
     in the UI.

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
- `PORT`: port to bind the HTTP server (default: 8080). On Render this is set automatically.

## Render Deployment

On Render, set the following environment variables:

| Variable | Description | Example |
|----------|-------------|---------|
| `GITSAFE_BASE_URL` | **Required.** The public URL of the deployed service. | `https://gitsafe.onrender.com` |
| `GITSAFE_GITHUB_CLIENT_ID` | GitHub OAuth App Client ID | `...` |
| `GITSAFE_GITHUB_CLIENT_SECRET` | GitHub OAuth App Client Secret | `...` |
| `GITSAFE_DRIVE_CLIENT_ID` | Google Drive OAuth Client ID | `...` |
| `GITSAFE_DRIVE_CLIENT_SECRET` | Google Drive OAuth Client Secret | `...` |

Register the GitHub and Google OAuth redirect URIs using your `GITSAFE_BASE_URL`:
- GitHub: `https://gitsafe.onrender.com/api/auth/github/callback`
- Google Drive: `https://gitsafe.onrender.com/api/auth/drive/callback`

The `PORT` variable is automatically provided by Render and does not need to be set manually.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is licensed under the MIT License - see the LICENSE file for details.