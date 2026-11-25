# Features

Checklist for the [TODO](./TODO) items plus the additional items from
[TODO2](./TODO2) and the other additions below. Each box is ticked in the
commit that implements the feature, on `features/auto`.

Tags: **[UI]** web UI · **[API]** HTTP API · **[AUTH]** authentication /
authorization · **[SANDBOXING]** isolation · **[DB]** storage · **[CLI]**
command line · **[SCHED]** scheduling · **[GIT]** git-backed collections ·
**[SSH]** SSH transport / credentials · **[TESTS]** tests · **[DOCS]** documentation · **[INFRA]** process model
(workers, queue, TLS, metrics)

## TODO

- [x] 1. [INFRA] Child worker processes with unix-socket IPC between the server and the workers (JSON-RPC)
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
- [x] 17. [API] API rework: only base layout `/api/v1/orgs/{org}/collections/{collection}` — orgs and collections as first-class entities (a collection is the current target, an org groups collections); the flat `/api/*` aliases were removed. Subsumes the former "versioned API" item.
- [x] 18. [AUTH] [API] Per-collection access restrictions (user-to-collection membership on the org/collection model, so the groups and ownership model from TODO2 can be layered on later)
- [x] 19. [SANDBOXING] Sandboxing: file-system isolation between collections (Landlock via go-landlock; deny ansible FS writes; keep one collection's credentials away from the others) + a validating playbook collection
- [x] 20. [API] [UI] Recent runs are read from `GET /api/v1/orgs/{org}/collections/{collection}/playbooks/{playbook}/hosts/{host}/runs?limit=N`; the standalone run detail endpoint and view were removed
- [x] 21. [API] The run listing, its filters and pagination were removed with the manual-run API; only `?limit=` on the per-host runs endpoint remains
- [x] 22. [UI] [SSH] UI for pending SSH host keys of one playbook/host, approved/rejected with `PATCH /api/v1/orgs/{org}/collections/{collection}/playbooks/{playbook}/hosts/{host}`; SSH pool stats endpoints were removed (the Monitor tab shows worker metrics from `/metrics`)
- [x] 23. [SCHED] Per-target or per-playbook robfig/cron v3 schedules instead of the single global interval
- [x] 25. [DB] Task history: keep the per-task results of every run
- [x] 26. [DB] [UI] ansible_facts: collect, save and display them — a jsonb blob per host, refreshed by runs
- [x] 27. [API] [UI] Run cancellation endpoints and the UI button were removed; graceful shutdown drains in-flight runs instead
- [x] 28. [API] [UI] Dry-run support (`--check`) remains in the runner; there is no HTTP endpoint to start a run
- [x] 29. [DB] Retention: a policy to purge old runs (max age and/or max count)
- [x] 30. [DB] [SCHED] Host auto clean-up (remove hosts not seen and not targeted after a configurable delay)
- [x] 31. [INFRA] TLS support for the HTTP server (cert/key in server.yml)
- [x] 32. [INFRA] `/metrics` endpoint (prometheus)
- [x] 33. [TESTS] Go tests for the server (API over httptest), applier and scheduler packages
- [x] 34. [UI] Host search box in the hosts view (name filter)
- [x] 35. [SSH] [AUTH] Ephemeral SSH certificates (short-lived, internal CA or a vault PKI / approle integration, pluggable in the ssh options)
- [x] 36. [API] [DOCS] Swagger / OpenAPI 3.1 documentation of the API, generated from swaggo/swag v2 annotations on the versioned routes and served from the server (e.g. `/api/docs`)
- [x] 37. [CLI] [API] CLI client: subcommands hitting `/api/v1` (`strangle-cli hosts`, `strangle-cli host <name> <target> --playbook NAME`, `strangle-cli hostkeys list --playbook NAME`, `hostkeys approve/reject <target> <host> --playbook NAME`) with a collection-scoped auth token from config; no `run`/`runs` commands

## Additional (beyond TODO)

- [x] [CLI] CLI shape: `strangle-cli <command> --flags` (no bare `strangle-cli` with flags) and a `strangle-server serve` command to launch the server
- [x] [CLI] [INFRA] Separate binaries: `cmd/strangle-cli` holds the client-side commands, `cmd/strangle-server` holds `serve` and `worker`

## Order notes

- **API layout rule:** apart from common/global endpoints (auth, health,
  org discovery, docs), every new HTTP endpoint must use
  `/api/v1/orgs/{org}/collections/{collection}/...`. Collection data and
  actions belong to the collection that owns them; flat `/api/*` routes were
  removed in feature 17, not a pattern for new work. Do not add new
  master-level or flat routes for collection-scoped features.
- **The API rework (17) is placed before the features that build on the
  new layout** — per-collection access restrictions (18) and the
  per-collection host/run endpoints (20, 21, 22, 27) — so they are implemented
  against `/api/v1/orgs/{org}/collections/{collection}` from the start.
  It also subsumes the former "versioned API (`/api/v1` prefix)" item.
- **36 (swagger) comes after 17** so the OpenAPI docs are generated
  against the versioned routes.
- From item 17 on, the item numbers no longer match the TODO line
  numbers one-to-one (the rework and ansible_facts were inserted from
  TODO2 and the versioned-API item was folded into the rework); items
  1–16 still map to TODO lines 1–32.
