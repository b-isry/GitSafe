# GitSafe Roadmap

This document outlines completed architectural milestones and planned future directions for GitSafe.

## Current Architecture

GitSafe provides disaster recovery backups of GitHub repositories to Google Drive or local storage.

Key components:
- Web dashboard: Go standard library HTTP router, server-rendered templates, and vanilla JavaScript.
- Authentication: GitHub OAuth sign-in with encrypted token storage.
- Storage backends: PostgreSQL for production multi-user deployments, or file-backed storage for local development.
- Backup engine: Sandboxed Git subprocess mirroring and bundling via `git bundle`, with isolated credential injection.
- Unattended backups: Scheduled daily backup endpoint protected by bearer token authentication.

## Planned Improvements

### 1. Granular GitHub Permissions (GitHub App Migration)
Currently GitSafe uses a GitHub OAuth App, which requires the broad `repo` scope to read private repositories.
Migrating to a GitHub App will allow requesting read-only repository contents permissions.

### 2. Destination Storage Options
- Additional cloud storage providers (such as S3-compatible object storage).
- Granular backup quota controls and retention limits on Google Drive.

### 3. Reliability and Operations
- Automatic retry with exponential backoff for transient Google Drive upload failures.
- Server-side session invalidation table in PostgreSQL.
- Configurable trusted proxy header support for rate limiting behind reverse proxies.
- Improved bundle naming that includes the repository owner to prevent name collisions.
