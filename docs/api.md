# API

All endpoints are served by the master process, next to the web UI, on the
address from `server.yml` (`listen_addr`, default `:8080`). Responses are
JSON; errors are `{"error": "<message>"}` with the corresponding status code.

**Per-collection restrictions.** Once at least one membership exists, a user
can only see and use collections they are a member of; the org/collection
pair from the URL is checked on scoped routes and the records returned by the
flat list endpoints are filtered. Provision memberships from the command
line (a database is required):

```
strangle-cli collection grant <user> <org> <collection>
strangle-cli collection revoke <user> <org> <collection>
strangle-cli collection list [--user <user>]
```

While no membership is configured every authenticated user can access every
collection, so adding the first UI account does not lock the UI down.
Groups and collection ownership can be layered on the same membership table
later.

## Versioned layout and aliases

The canonical API is versioned under `/api/v1`. Every old flat `/api/*` route
is still registered as an alias, so clients do not have to migrate at once:
`GET /api/runs` and `GET /api/v1/runs` are the same handler. Authentication,
tokens and webhook rules apply identically to both prefixes.

Collections (the targets from `collection.yml`) are first-class resources
grouped into organizations. An org comes from the optional `org` field of a
collection entry and defaults to `default`:

```yaml
collections:
  - name: homelab                # /api/v1/orgs/default/collections/homelab
    path: ./ansible
  - name: edge
    org: lab                     # /api/v1/orgs/lab/collections/edge
    path: ./edge-ansible
```

Org and collection resources exist only under the versioned prefix:

- `GET /api/v1/orgs` — all orgs, each with its collections and their playbooks.
- `GET /api/v1/orgs/{org}` — one org.
- `GET /api/v1/orgs/{org}/collections` — the collections of one org.
- `GET /api/v1/orgs/{org}/collections/{collection}` — one collection, with
  its playbooks and links to its sub-resources.

Collection-scoped aliases are available for the data endpoints:
`runs`, `runs/{id}`, `hosts`, `hosts/{host}/runs`, `hosts/{host}/changes`,
`playbooks`, `stats`, `hostkeys`, `ssh/stats`, and `POST run` / `POST sync`.
For example:

```
GET  /api/v1/orgs/lab/collections/edge/runs
GET  /api/v1/orgs/lab/collections/edge/runs/1234
GET  /api/v1/orgs/lab/collections/edge/hosts
POST /api/v1/orgs/lab/collections/edge/run
```

On collection-scoped `run` and `sync` routes the collection in the URL selects
the target; a conflicting `target` field in the JSON body is rejected with
`400`. An unknown org/collection pair returns `404`.

## OpenAPI / Swagger

The machine-readable OpenAPI 3.1 document is generated at build time by
`swaggo/swag` v2 from annotations on the versioned API in
`internal/server/openapi_annotations.go`. The canonical paths (not the legacy
flat aliases) are documented:

- `GET /api/v1/openapi.json` — the generated OpenAPI 3.1 JSON document.
- `GET /api/docs` — Swagger UI, initialized from the document above.

Both are public, even when UI authentication is on (including the Swagger UI
assets under `/api/docs/`): they describe the API but expose no run, host or
collection data themselves. Operation schemas are
derived from the request/response Go types referenced by the annotations.

Regenerate the checked-in `docs/` output after changing a route or its
annotations:

```sh
make swag        # swag init --v3.1 ...
make swag-check  # regenerate and fail if docs/ is out of date
```

The web layer itself is served by `github.com/labstack/echo/v5`; the legacy
flat `/api/*` aliases are registered in Echo alongside the versioned routes.

The `strangle-cli` client (`strangle-cli run|runs|hosts|host|hostkeys`) is generated
against the same versioned routes: it resolves a target's org from
collection.yml and only then calls a
`/api/v1/orgs/{org}/collections/{collection}/...` path. It sends a user bearer
token from `auth.cli_token` or `--token`; the static sync token is not used
outside `POST /api/sync`.

## Authentication

There are two independent mechanisms:

**UI authentication (session cookie).** When at least one local account
exists (see [configuration](configuration.md#ui-authentication)), every
`/api/*` request needs a valid session cookie, except the open endpoints:
`GET /api/health`, `POST /api/login`, `POST /api/logout`, `GET /api/me`,
`GET /api/docs` (and its assets under `/api/docs/`) and
`GET /api/v1/openapi.json`. Sessions live 12 hours and are
held in the master's memory (a master restart logs everyone out). Static UI
files are always served; the app
itself gates on `GET /api/me`.

**Sync token (webhooks).** Independently of UI authentication, when
`auth.token` is set `POST /api/sync` accepts
`Authorization: Bearer <token>` or, webhook-friendly,
`X-Strangle-Token: <token>` (the bearer header wins when both are present;
the comparison is constant-time). A valid UI session also works on
`/api/sync`.

**User API tokens (bearer).** Local accounts can create their own bearer
tokens (in the web UI under *Tokens* or with
`strangle-cli token`, see [configuration](configuration.md#api-tokens)). A
valid token authenticates **every** `/api/*` request as its owner, sent as
`Authorization: Bearer st_…` or, webhook-friendly,
`X-Strangle-Token: st_…` (the bearer header wins when both are present).
Only the SHA-256 digest of a token is stored; the raw value is returned
exactly once at creation. The static sync token (`auth.token`) is **not** a
user account: it only opens `POST /api/sync` and is rejected with `401`
everywhere else when authentication is on.

## GET /api/health

Liveness. Open, even when UI authentication is on.

```json
{"status": "ok"}
```

## GET /metrics

Prometheus text exposition (`text/plain; version=0.0.4`), open like
`/api/health` so scrapers need no UI session. Only aggregate values are
exported: run/host/worker counters and gauges from the store plus a small set
of Go runtime metrics. No host names, playbook output or credentials appear
in labels.

```
# HELP strangle_runs Number of recorded runs.
# TYPE strangle_runs gauge
strangle_runs 42
# HELP strangle_workers Number of worker slots by state.
# TYPE strangle_workers gauge
strangle_workers{state="alive"} 2
```

Run counts are gauges rather than counters: retention may purge old rows and
reduce them between scrapes. The full set includes `strangle_build_info`,
`strangle_runs`, `strangle_runs_succeeded`, `strangle_runs_failed`,
`strangle_run_duration_ms_avg`, `strangle_last_run_timestamp_seconds`,
`strangle_hosts{state=...}`,
`strangle_playbook_runs{collection,playbook}`,
`strangle_playbook_failed_runs{collection,playbook}`,
`strangle_workers{state=...}`, `strangle_worker_jobs{worker,collection}`,
`strangle_worker_restarts_total{worker,collection}`, `strangle_store_up`,
`process_start_time_seconds` and the common `go_*` runtime gauges.

## POST /api/login

Authenticate a local account and start a session. The session cookie
(`strangle_session`, HttpOnly, SameSite=Lax, 12 h) is set on success.

```json
{"username": "admin", "password": "..."}
```

`200` returns the same body as `GET /api/me`. Errors: `400` invalid JSON,
`401` `invalid username or password` (returned for both an unknown username
and a wrong password, to prevent account enumeration), and `401`
`authentication is not enabled: no users are configured (strangle-cli user add
<name> --password <pw>)` when no account exists.

## POST /api/logout

End the session carried by the request cookie and clear the cookie. Always
`200`:

```json
{"status": "logged out"}
```

## GET /api/me

Answer "who is logged in?" and double as the UI's probe for whether
authentication is on at all:

- `200 {"authenticated": false, "username": null}` — authentication is off
  (open server).
- `200 {"authenticated": true, "username": "admin"}` — valid session.
- `401 {"error": "not authenticated"}` — authentication is on and no valid
  session cookie is present (the UI shows its login screen).

## API tokens (self-service)

These endpoints manage the caller's own bearer tokens. They require a user
identity — a valid session cookie or a valid user token — so a request can
also be made with a token. A request that is neither a user session nor a user
token gets `403`.

### GET /api/tokens

List the caller's tokens, newest name first. Only a display prefix of the
raw value is returned — the full value can never be retrieved again.

```json
[
  {"name": "ci", "prefix": "st_9a1b2c3d", "created_at": "2026-11-24T09:12:00Z",
   "last_used_at": "2026-11-24T10:30:00Z"},
  {"name": "laptop", "prefix": "st_44f0c2aa", "created_at": "2026-11-20T08:00:00Z",
   "expires_at": "2026-11-27T08:00:00Z"}
]
```

`expires_at` is omitted for a token that never expires; `last_used_at` is
omitted until the token has been used at least once. Expired tokens are
pruned and not listed.

### POST /api/tokens

Create a token. `ttl_hours` is optional; `0` or absent means no expiry.

```sh
curl -X POST localhost:8080/api/tokens \
  -H "Cookie: strangle_session=…" \
  -d '{"name": "ci", "ttl_hours": 72}'
```

`201` returns the raw value — the **only** time it is shown:

```json
{"raw": "st_9a1b2c3d…", "token": {"name": "ci", "prefix": "st_9a1b2c3d", "created_at": "…"}}
```

Errors: `400` invalid JSON or an empty name, `409` `token "name" already
exists` (names are unique per user).

### DELETE /api/tokens/{name}

Revoke the named token. `204` on success, `404` if no such token exists for
the caller.

## GET /api/stats

Fleet statistics, derived from the stored history.

```json
{
  "hosts": {
    "total": 12,
    "in_inventory": 12,
    "active": 11,
    "disappeared": 1
  },
  "runs": {
    "total": 340,
    "succeeded": 335,
    "failed": 5,
    "avg_duration_ms": 68500,
    "last_run_at": "2026-11-24T23:21:49Z"
  },
  "per_playbook": [
    {"target": "home", "playbook": "hypervisors", "total": 170, "failed": 1, "last_run_at": "..."}
  ]
}
```

## GET /api/runs

Runs, newest first. `?limit=` defaults to 50. Each run:

```json
{
  "id": 1234,
  "target": "home",
  "playbook": "hypervisors",
  "started_at": "...",
  "finished_at": "...",
  "duration_ms": 68500,
  "exit_code": 0,
  "success": true,
  "hosts": 8,
  "changed_hosts": 2,
  "failed_hosts": 0
}
```

This is the listing the UI polls: it deliberately does **not** include the
captured output. Use the detail endpoint below to read it.

### Filtering and pagination

The run listing accepts these query parameters (on the flat and the
collection-scoped routes alike):

| Parameter | Meaning |
|-----------|---------|
| `playbook` | Only runs of this playbook name. |
| `host` | Only runs that included this host name. |
| `from` | RFC3339 timestamp; only runs started at or after it. |
| `to` | RFC3339 timestamp; only runs started before it (exclusive). |
| `limit` | Page size, default 50. |
| `offset` | Number of matching runs to skip, default 0. |

The total number of matching, visible runs is returned in the
`X-Total-Count` response header (before `offset`/`limit` are applied). An
invalid timestamp or a `to` not after `from` is answered with `400`.

```
GET /api/v1/orgs/lab/collections/edge/runs?playbook=site&host=web1
GET /api/v1/orgs/lab/collections/edge/runs?from=2026-11-24T10:00:00Z&to=2026-11-25T00:00:00Z
GET /api/v1/orgs/lab/collections/edge/runs?limit=25&offset=50
```

## GET /api/v1/orgs/{org}/collections/{collection}/runs/{id}

One run, including its full `output`. Flat aliases: `GET /api/v1/runs/{id}`
and `GET /api/runs/{id}`. On a collection-scoped path a run belonging to a
different collection is answered with `404`, so guessing an ID does not leak
another collection's log. Unknown IDs are `404`, malformed IDs are `400`.

```json
{
  "id": 1234,
  "target": "home",
  "playbook": "hypervisors",
  "started_at": "...",
  "finished_at": "...",
  "duration_ms": 68500,
  "exit_code": 0,
  "success": true,
  "hosts": 8,
  "changed_hosts": 2,
  "failed_hosts": 0,
  "output": "PLAY [all] ***\n...\n"
}
```

## POST /api/v1/orgs/{org}/collections/{collection}/runs/{id}/cancel

Cancels a queued or running playbook run. There is deliberately no flat alias:
a run belongs to one collection and the membership check must use that
collection. A run from another collection returns `404`; a run that already
finished returns `409`. On success the response is
`{"id":1234,"state":"cancelled"}`. The run record is kept, with `state`
`cancelled` and `canceled: true`, and its captured output records the
cancellation.

```sh
curl -X POST http://localhost:8080/api/v1/orgs/default/collections/home/runs/1234/cancel \
  -H 'Cookie: strangle_session=...'
```

## GET /api/v1/orgs/{org}/collections/{collection}/runs/{id}/tasks

Every per-task result recorded for one run, in task order. Unlike
`/api/hosts/{host}/changes` (the latest state of a task), this endpoint is an
append-only history: repeated task names and older runs remain available.
There is deliberately no flat alias. A run from another collection returns
`404`; collection membership still applies.

```json
[
  {
    "id": 9102,
    "run_id": 1234,
    "target": "home",
    "host": "web-01",
    "task": "configure nginx",
    "status": "changed",
    "diff": "@@ -1,2 +1,2 @@\n-worker_connections auto;\n+worker_connections 512;\n",
    "error": "",
    "finished_at": "..."
  },
  {
    "id": 9103,
    "run_id": 1234,
    "target": "home",
    "host": "web-01",
    "task": "check nginx",
    "status": "ok",
    "finished_at": "..."
  }
]
```

## GET /api/hosts

All hosts with their last status and disappearance state. Hosts are scoped
per target: the same name in two targets is two entries.

```json
[
  {
    "target": "home",
    "name": "web-01",
    "ip": "10.0.0.5",
    "user": "kakwa",
    "groups": ["web"],
    "in_inventory": true,
    "active": true,
    "first_seen_at": "...",
    "last_seen_at": "...",
    "last_run_at": "...",
    "last_status": "ok",
    "last_change_at": "...",
    "facts": {"ansible_hostname": "web-01", "ansible_os_family": "Debian"}
  }
]
```

`last_status` is one of `ok`, `changed`, `failed`, `unreachable` (empty until
the first run). A host that was in scope of a run but missing from the recap
gets `disappeared_at` set; `active` is true while that condition holds.

`facts` is the latest `ansible_facts` blob collected for the host, returned as
a JSON object (omitted when no facts have been gathered). It is refreshed only
by a later run that actually returns facts. The canonical collection-scoped
`GET /api/v1/orgs/{org}/collections/{collection}/hosts` route returns the same
host shape, filtered by collection access.

## GET /api/hosts/{host}/runs

The last runs that included the host (matched by name). `?limit=` defaults to
10. Each entry is the run (as in `/api/runs`) joined with the host's recap
line:

```json
{
  "run_id": 1234,
  "target": "home",
  "host": "web-01",
  "playbook": "hypervisors",
  "started_at": "...",
  "duration_ms": 68500,
  "exit_code": 0,
  "success": true,
  "status": "changed",
  "ok": 12, "changed": 1, "failed": 0, "unreachable": 0,
  "skipped": 0, "rescued": 0, "ignored": 0
}
```

## GET /api/hosts/{host}/changes

The host's task traces, newest first. `?limit=` defaults to 200. Each entry
is the latest state of one task on the host, with the last diff and error:

```json
{
  "target": "home",
  "host": "web-01",
  "task": "configure nginx",
  "status": "changed",
  "last_run_id": 1234,
  "last_run_at": "...",
  "last_changed_at": "...",
  "last_diff": "@@ -1,2 +1,2 @@\n-worker_connections auto;\n+worker_connections 512;\n",
  "last_error": ""
}
```

## GET /api/playbooks

The configured jobs, keyed by target name, each with its latest run outcome:

```json
{
  "home": [
    {"name": "hypervisors", "playbook": "hypervisor.yml", "inventory": "inventory/hypervisor.yml",
     "last_status": "ok", "last_run_at": "..."},
    {"name": "base_infra", "playbook": "infra.yml", "inventory": "inventory/infra.yml"}
  ]
}
```

`last_status` is `ok` or `failed`; both are omitted for a job that has never
run.

## GET /api/workers

The worker pool: one entry per worker slot. A slot is the fixed place in the
pool; its process (and PID) may be replaced by a restart.

```json
[
  {"id": 0, "pid": 4242, "socket": "/tmp/.../worker-0.sock",
   "alive": true, "jobs": 0, "restarts": 0}
]
```

`jobs` is the number of in-flight runs; `alive` is the result of the last
health probe.

## POST /api/v1/orgs/{org}/collections/{collection}/run

Start a playbook run in the background. `playbook` is the **job name** from
`strangle.yml`, not a file path. Unknown targets or job names are rejected
with `404`. The legacy flat alias is `POST /api/run`; both accept the same
body, but the collection-scoped URL is canonical and is what the UI uses.

Set `"check": true` to run Ansible with `--check` (dry run). The flag is
recorded on the run and shown in the run detail view. Scheduled runs never
set it.

```sh
curl -X POST localhost:8080/api/v1/orgs/default/collections/home/run \
  -d '{"target": "home", "playbook": "hypervisors", "check": true}'
```

```json
{"status": "queued", "target": "home", "playbook": "hypervisors", "run_id": 1234, "check": true}
```

`status` is always `queued`: the API creates the run record and enqueues a
River job. Runs are visible immediately in `/api/runs` with `state` `queued`,
`running`, `completed` or `cancelled`.

## POST /api/sync

Re-sync one git-backed target: move its checkout to its tracked ref, or to
the given branch (which becomes the new tracked ref), and reload its
`strangle.yml`. The applier's view of the target's playbooks is refreshed,
so a branch switch or a new `strangle.yml` takes effect from the next run.
See [git-backed collections](git-collections.md).

```sh
curl -X POST localhost:8080/api/sync \
  -H "X-Strangle-Token: <token>" \
  -d '{"target": "site", "branch": "release-1.4"}'
```

```json
{"target": "site", "rev": "9f2c1ab...", "changed": true}
```

Errors: `401` when the request carries neither a valid UI session nor a
matching sync token (when `auth.token` is set); with no users and no token
the endpoint is open. Otherwise `404` unknown target, `400` target without
a git upstream, `502` a git failure (network, auth, broken
`strangle.yml`). When no git-backed target is configured, the endpoint
returns `404`.

## Host keys (SSH transport only)

These endpoints apply to jobs using the Go-managed SSH transport with the
`api` host-key policy. They are collection-scoped: every path carries
`/api/v1/orgs/{org}/collections/{collection}` and is checked against the
caller's collection membership. There is no master-level or flat `/api/*`
route for SSH administration. In requests the `target` field is the SSH scope
key `<target>/<playbook name>`; its first component must be the collection in
the URL, or the request is answered with `400`. The UI's **SSH** screen reads
each visible collection and sends approve/reject through that collection's
URL.

### GET /api/v1/orgs/{org}/collections/{collection}/hostkeys

Host keys pending approval for this collection. `status` is `pending` for an
unknown key and `rejected` for a changed key on an approved host; both are
resolved with accept or reject below.

```json
[
  {"target": "site/prod", "host": "10.0.0.9",
   "fingerprint": "SHA256:...", "public_key": "ssh-ed25519 AAAA...",
   "status": "pending", "first_seen": "..."}
]
```

### POST /api/v1/orgs/{org}/collections/{collection}/hostkeys/accept

```json
{"target": "site/prod", "host": "10.0.0.9", "fingerprint": "SHA256:..."}
```

Approves the pending key (the fingerprint must match a pending one) and
persists it for future connections.

```json
{"status": "accepted", "host": "10.0.0.9"}
```

### POST /api/v1/orgs/{org}/collections/{collection}/hostkeys/reject

```json
{"target": "site/prod", "host": "10.0.0.9"}
```

Removes the pending key; the connection stays refused until re-approved.

```json
{"status": "rejected", "host": "10.0.0.9"}
```

### GET /api/v1/orgs/{org}/collections/{collection}/ssh/stats

Counters of the SSH managers of this collection, keyed by scope:

```json
{
  "site/prod": {"active_connections": 0, "total_dials": 0},
  "site/common": {"active_connections": 1, "total_dials": 4}
}
```

The managers of the process serving this API exist for the host-key approval
endpoints; the playbook's actual SSH connections are pooled inside the worker
process that runs it.
