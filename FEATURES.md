# Features

Checklist for the [TODO](./TODO) items plus the additional items from
[TODO2](./TODO2) and the other additions below. Each box is ticked in the
commit that implements the feature, on `features/auto`.

Tags: **[UI]** web UI · **[API]** HTTP API · **[AUTH]** authentication /
authorization · **[SANDBOXING]** isolation · **[DB]** storage · **[CLI]**
command line · **[SCHED]** scheduling · **[GIT]** git-backed collections ·
**[SSH]** SSH transport / credentials · **[NOTIFY]** notifications ·
**[TESTS]** tests · **[DOCS]** documentation · **[INFRA]** process model
(workers, queue, TLS, metrics)

## TODO

- [x] 1. [INFRA] Child worker processes with unix-socket IPC between the server and the workers (gRPC)
- [x] 2. [INFRA] [DB] Job queueing with river (postgres-backed job queue)
- [x] 3. [INFRA] [SCHED] Master server / scheduler and basic worker manager (picks jobs through the postgres DB)
- [x] 4. [GIT] [API] Git-backed collections: webhook + auth token for updates, branch handling and branch templating in strangle.yml
- [x] 5. [UI] Playbooks screen: one table per collection
- [x] 6. [UI] Color the diffs
- [x] 7. [UI] Remove the "polling every 10s" badge
- [x] 8. [UI] Host view: don't repeat the host per run
- [x] 9. [UI] Dark mode
- [x] 10. [UI] Code blocks and colored diffs where applicable
- [x] 11. [UI] Bookmarkable URLs (react-router)
- [x] 12. [DOCS] README cleanup and start of docs/
- [x] 13. [UI] Inspect each screen for issues (broken links, etc.)
- [x] 14. [TESTS] [UI] Cypress tests
- [x] 15. [AUTH] [UI] UI authentication (local accounts; LDAP/OAuth as later options)
- [x] 16. [AUTH] [API] API / token self-service (for user-linked CLIs or webhooks / integrations)
- [x] 17. [API] API rework: base layout `/api/v<>/orgs/{org}/collections/{collection}` — orgs and collections as first-class entities (a collection is the current target, an org groups collections), versioned prefix with the current flat routes kept as aliases. Subsumes the former "versioned API" item.
- [x] 18. [AUTH] [API] Per-collection access restrictions (user-to-collection membership on the org/collection model, so the groups and ownership model from TODO2 can be layered on later)
- [x] 19. [SANDBOXING] Sandboxing: file-system isolation between collections (Landlock via go-landlock; deny ansible FS writes; keep one collection's credentials away from the others) + a validating playbook collection
- [x] 20. [API] [UI] Run detail: `GET /api/v1/orgs/{org}/collections/{collection}/runs/{id}` and a run view with the full captured output; stop inlining the full output of the last 50 runs into the collection's runs listing on every poll
- [x] 21. [API] Filter a collection's runs (`/api/v1/orgs/{org}/collections/{collection}/runs`) by playbook, host and time range (+ pagination)
- [x] 22. [UI] [SSH] UI for pending SSH host keys (approve/reject) and SSH pool stats — collection-scoped versioned endpoints under `/api/v<>/orgs/{org}/collections/{collection}`
- [x] 23. [SCHED] Per-target or per-playbook interval instead of the single global interval (ideally cron-like)
- [x] 24. [NOTIFY] Notify on run failure (webhook, email, slack or similar), configured per collection
- [x] 25. [DB] Task history: keep the per-task results of every run
- [x] 26. [DB] [UI] ansible_facts: collect, save and display them — a jsonb blob per host, refreshed by runs
- [x] 27. [API] [UI] Cancel a running run (`POST /api/v1/orgs/{org}/collections/{collection}/runs/{id}/cancel` + UI button)
- [ ] 28. [API] [UI] Check mode: run a playbook with `--check` (dry run) from the UI or API
- [ ] 29. [DB] Retention: a policy to purge old runs (max age and/or max count)
- [ ] 30. [DB] [SCHED] Host auto clean-up (remove hosts not seen and not targeted after a configurable delay)
- [ ] 31. [INFRA] TLS support for the HTTP server (cert/key in server.yml)
- [ ] 32. [INFRA] `/metrics` endpoint (prometheus)
- [ ] 33. [TESTS] Go tests for the server (API over httptest), applier and scheduler packages
- [ ] 34. [UI] Host search box in the hosts view (name filter)
- [ ] 35. [SSH] [AUTH] Ephemeral SSH certificates (short-lived, internal CA or a vault PKI / approle integration, pluggable in the ssh options)
- [ ] 36. [API] [DOCS] Swagger / OpenAPI documentation of the API, generated against the versioned routes and served from the server (e.g. `/api/docs`)
- [ ] 37. [CLI] [API] CLI client: subcommands hitting the versioned API (`strangle run <target> <playbook>`, `strangle runs|hosts|host <name>`, `strangle hostkeys list/accept/reject`) with the auth token from config

## Additional (beyond TODO)

- [x] [CLI] CLI shape: `strangle <command> --flags` (no bare `strangle` with flags) and a `strangle serve` command to launch the server

## Order notes

- **API layout rule:** apart from common/global endpoints (auth, token
  self-service, org discovery), every new HTTP endpoint must use
  `/api/v<>/orgs/{org}/collections/{collection}/...`. Collection data and
  actions belong to the collection that owns them; the flat `/api/*` routes
  are legacy aliases from feature 17, not a pattern for new work. Do not add
  new master-level or flat routes for collection-scoped features.
- **The API rework (17) is placed before the features that build on the
  new layout** — per-collection access restrictions (18) and the
  per-collection run endpoints (20, 21, 27) — so they are implemented
  against `/api/v<>/orgs/{org}/collections/{collection}` from the start.
  It also subsumes the former "versioned API (`/api/v1` prefix)" item.
- **36 (swagger) comes after 17** so the OpenAPI docs are generated
  against the versioned routes.
- From item 17 on, the item numbers no longer match the TODO line
  numbers one-to-one (the rework and ansible_facts were inserted from
  TODO2 and the versioned-API item was folded into the rework); items
  1–16 still map to TODO lines 1–32.
