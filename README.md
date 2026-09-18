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

3. Configure GitHub OAuth (required for the Cloud Repositories feature):
   - Register a GitHub OAuth App at https://github.com/settings/applications/new
     - Homepage URL: `http://127.0.0.1:8080`
     - Authorization callback URL: `http://127.0.0.1:8080/api/auth/github/callback`
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

4. Configure Google Drive OAuth (required to back repositories up to Google
   Drive):
   - Create a Google Cloud OAuth 2.0 **Web application** client:
     - Go to https://console.cloud.google.com/apis/credentials
     - Click **Create Credentials** → **OAuth client ID**, choose
       **Web application**.
     - Under **Authorized redirect URIs**, add exactly:
       `http://127.0.0.1:8080/api/auth/drive/callback`
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
   - The redirect URI used by GitSafe is `http://127.0.0.1:8080/api/auth/drive/callback`
     (the server binds `127.0.0.1:8080`). If you bind the server elsewhere, set
     `driveOAuth.redirectUrl` in `config.yaml` to match both your bind address
     and the URI you registered in Google Cloud.

## Usage

```bash
go run ./cmd/server
```

Then open http://127.0.0.1:8080 and click **Connect GitHub** on the Cloud
Repositories page to authorize through GitHub's OAuth flow, then **Connect
Google Drive** (from the Cloud Repositories page). Backups are
uploaded to the connected account's Google Drive.

## Configuration

- `config.yaml`: server settings (days, outputPath, cloud, github, driveOAuth)
- `.env`: deployment credentials (`GITSAFE_GITHUB_CLIENT_ID`,
  `GITSAFE_GITHUB_CLIENT_SECRET`, `GITSAFE_DRIVE_CLIENT_ID`,
  `GITSAFE_DRIVE_CLIENT_SECRET`)

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is licensed under the MIT License - see the LICENSE file for details. 