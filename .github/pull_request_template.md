## What and why

<!-- What does this change, and why? Link the issue: "Fixes #123". -->

## How it was tested

<!-- Commands run, new or updated tests, manual steps. -->

## Checklist

- [ ] The title follows [Conventional Commits](https://www.conventionalcommits.org/) (`fix: …`, `feat: …`, `docs: …`)
- [ ] `make lint test race` passes (and `make test-postgres` if storage changed)
- [ ] Tests added or updated for behavior changes, with no network or real credentials needed
- [ ] Docs, `.env.example` and `render.yaml` updated for new configuration
- [ ] No tokens, secrets or OAuth codes are logged, displayed or committed
- [ ] Security-sensitive change (auth, sessions, tokens, cron)? Described above for reviewers
