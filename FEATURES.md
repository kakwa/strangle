# Features

Checklist for the [TODO](./TODO) items (plus the additional items below).
Each box is ticked in the commit that implements the feature, on
`features/auto`.

## TODO

- [x] 1. Child worker processes with unix-socket IPC between the server and the workers (gRPC)
- [x] 2. Job queueing with river (postgres-backed job queue)
- [x] 3. Master server / scheduler and basic worker manager (picks jobs through the postgres DB)
- [x] 4. Git-backed collections: webhook + auth token for updates, branch handling and branch templating in strangle.yml
- [x] 5. Playbooks screen: one table per collection
- [x] 6. Color the diffs
- [x] 7. Remove the "polling every 10s" badge
- [x] 8. Host view: don't repeat the host per run
- [x] 9. Dark mode
- [x] 10. Code blocks and colored diffs where applicable
- [x] 11. Bookmarkable URLs (react-router)
- [x] 12. README cleanup and start of docs/
- [x] 13. Inspect each screen for issues (broken links, etc.)
- [x] 14. Cypress tests
- [x] 15. UI authentication (local accounts; LDAP/OAuth as later options)
- [x] 16. API / token self-service (for user-linked CLIs or webhooks / integrations)
- [ ] 17. Per-collection access restrictions
- [ ] 18. Sandboxing: file-system isolation between collections (Landlock via go-landlock; deny ansible FS writes; keep one collection's credentials away from the others) + a validating playbook collection
- [ ] 19. Run detail: `GET /api/runs/{id}` and a run view with the full captured output; stop inlining the full output of the last 50 runs into `/api/runs` on every poll
- [ ] 20. Filter `/api/runs` by target, playbook, host and time range (+ pagination)
- [ ] 21. UI for pending SSH host keys (approve/reject) and SSH pool stats
- [ ] 22. Per-target or per-playbook interval instead of the single global interval (ideally cron-like)
- [ ] 23. Notify on run failure (webhook, email, slack or similar)
- [ ] 24. Task history: keep the per-task results of every run
- [ ] 25. Cancel a running run (API endpoint + UI button)
- [ ] 26. Check mode: run a playbook with `--check` (dry run) from the UI or API
- [ ] 27. Retention: a policy to purge old runs (max age and/or max count)
- [ ] 28. Host auto clean-up (remove hosts not seen and not targeted after a configurable delay)
- [ ] 29. TLS support for the HTTP server (cert/key in server.yml)
- [ ] 30. `/metrics` endpoint (prometheus)
- [ ] 31. Go tests for the server (API over httptest), applier and scheduler packages
- [ ] 32. Host search box in the hosts view (name filter)
- [ ] 33. Ephemeral SSH certificates (short-lived, internal CA or a vault PKI / approle integration, pluggable in the ssh options)
- [ ] 34. Swagger / OpenAPI documentation of the API, served from the server (e.g. `/api/docs`)
- [ ] 35. Versioned API: `/api/v1` prefix (current routes kept as aliases)
- [ ] 36. CLI client: subcommands hitting the API (`strangle run <target> <playbook>`, `strangle runs|hosts|host <name>`, `strangle hostkeys list/accept/reject`) with the auth token from config

## Additional (beyond TODO)

- [x] CLI shape: `strangle <command> --flags` (no bare `strangle` with flags) and a `strangle serve` command to launch the server
