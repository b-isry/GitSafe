# Security Policy

GitSafe handles OAuth tokens for GitHub and Google Drive, so we take security
reports seriously.

## Reporting a vulnerability

**Please do not open a public issue, discussion or pull request for security
problems.**

Report privately through GitHub Security Advisories:

1. Go to <https://github.com/b-isry/gitsafe/security/advisories/new>
   (the repository's **Security** tab → **Report a vulnerability**).
2. Describe the issue, the affected version or commit, the impact, and the
   steps to reproduce. Include a proof of concept if you have one.

Please include as much as you can, but **never include real tokens or
secrets** of other people.

What to expect:

- An acknowledgement within **7 days**.
- An assessment and a planned fix timeline after triage.
- Credit in the advisory once a fix is released, if you want it.

Please give us a reasonable amount of time to fix the issue before any public
disclosure. We follow coordinated disclosure through the published advisory.

## Supported versions

GitSafe is pre-1.0 and deployed from `main`. Only the latest `main` receives
security fixes.

## Scope

In scope:

- The GitSafe server and web UI in this repository: authentication, sessions,
  CSRF, OAuth state, token storage and encryption, access control between
  users, the cron endpoint, and backup handling.

Out of scope:

- Vulnerabilities in GitHub, Google or Render themselves.
- Denial of service through volume alone.
- Reports from automated scanners without a demonstrated impact.
- Missing hardening headers without an exploit.
