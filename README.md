# GitSafe

GitSafe is a tool that helps you manage and backup your Git repositories. It identifies stale repositories and creates backups of them in Google Drive.

## Features

- Scan directories for Git repositories
- Identify repositories that haven't been updated in a specified time period
- Create zip archives of repositories
- Upload repository backups to Google Drive
- Progress tracking during uploads

## Installation

1. Clone the repository:
```bash
git clone https://github.com/b-isry/gitsafe.git
cd gitsafe
```

2. Install dependencies:
```bash
go mod download
```

3. Set up Google Drive API:
   - Go to [Google Cloud Console](https://console.cloud.google.com)
   - Create a new project or select an existing one
   - Enable the Google Drive API
   - Create a service account
   - Download the service account credentials as `credentials.json`
   - Place `credentials.json` in the project root

## Usage

```bash
go run main.go --root <path> [--days <n>] [--out <path>] [--cloud]

```

## Configuration

- `credentials.json`: Google Drive API credentials file

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is licensed under the MIT License - see the LICENSE file for details. 