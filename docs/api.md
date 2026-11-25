# API

All endpoints are served by the master process, next to the web UI, on the
address from `server.yml` (`listen_addr`, default `:8080`). Responses are
JSON; errors are `{"error": "<message>"}` with the corresponding status code.

## Versioned layout

**Every API route lives under `/api/v1`.** The old flat `/api/*` aliases have
been removed: `GET /api/runs` and similar paths no longer exist. Documentation
is served at `/api/v1/openapi.json` and `/api/docs`; `/metrics` is a separate
public monitoring endpoint, not part of the API.

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

The complete route table:

```
GET    /api/v1/health
POST   /api/v1/login
POST   /api/v1/logout
GET    /api/v1/me
GET    /api/v1/openapi.json
GET    /api/docs                 (Swagger UI; assets under /api/docs/)

GET    /api/v1/orgs
POST   /api/v1/orgs
GET    /api/v1/orgs/{org}
GET    /api/v1/orgs/{org}/collections
GET    /api/v1/orgs/{org}/collections/{collection}

GET    /api/v1/orgs/{org}/collections/{collection}/hosts
GET    /api/v1/orgs/{org}/collections/{collection}/playbooks
POST   /api/v1/orgs/{org}/collections/{collection}/hook
GET    /api/v1/orgs/{org}/collections/{collection}/tokens
POST   /api/v1/orgs/{org}/collections/{collection}/tokens
DELETE /api/v1/orgs/{org}/collections/{collection}/tokens/{name}

GET    /api/v1/orgs/{org}/collections/{collection}/playbooks/{playbook}/hosts/{host}/hostkeys
PATCH  /api/v1/orgs/{org}/collections/{collection}/playbooks/{playbook}/hosts/{host}
GET    /api/v1/orgs/{org}/collections/{collection}/playbooks/{playbook}/hosts/{host}/runs?limit=N
```

An unknown org/collection pair returns `404` on every scoped route. A token
created for a different pair is rejected there (`403`).

**Per-collection restrictions.** Once at least one membership exists, a user
can only see and use collections they are a member of; the org/collection
pair from the URL is checked on scoped routes and the org listing is filtered.
Provision memberships from the command line (a database is required):

```
strangle-cli collection grant <user> <org> <collection>
strangle-cli collection revoke <user> <org> <collection>
strangle-cli collection list [--user <user>]
```

While no membership is configured every authenticated user can access every
collection, so adding the first UI account does not lock the UI down. Groups
and collection ownership can be layered on the same membership table later.

## OpenAPI / Swagger

The machine-readable OpenAPI 3.1 document is generated at build time by
`swaggo/swag` v2 from annotations on the versioned API in
`internal/server/handlers`. Only the canonical `/api/v1` routes are documented:

- `GET /api/v1/openapi.json` — the generated OpenAPI 3.1 JSON document.
- `GET /api/docs` — Swagger UI, initialized from the document above.

Both are public, even when UI authentication is on (including the Swagger UI
assets under `/api/docs/`): they describe the API but expose no collection
data themselves. Documented schemas are derived from the request/response Go
types referenced by the annotations.

Regenerate the checked-in `docs/` output after changing a route or its
annotations:

```sh
make swag        # swag init --v3.1 ...
make swag-check  # regenerate and fail if docs/ is out of date
```

The HTTP layer uses `github.com/labstack/echo/v5`. The `strangle-cli` API-backed
commands (`strangle-cli hosts`, `host`, `hostkeys`) call only the canonical
`/api/v1/orgs/{org}/collections/{collection}/...` paths. They resolve a
target's org from `collection.yml` (or `--org`) and send a user bearer token
from `auth.cli_token` or `--token`; the static hook token is not used elsewhere.

## Authentication

There are three independent ways to authenticate:

**UI authentication (session cookie).** When at least one local account
exists (see [configuration](configuration.md#ui-authentication)), every
`/api/v1` request needs a valid session cookie or a user bearer token (see
below), except the open endpoints:
`GET /api/v1/health`, `POST /api/v1/login`, `POST /api/v1/logout`,
`GET /api/v1/me`, `GET /api/v1/openapi.json`, and `GET /api/docs` (and its
assets under `/api/docs/`). Sessions live 12 hours and are held in the
master's memory (a master restart logs everyone out). Static UI files are
always served; the app itself gates on `GET /api/v1/me`.

**User API tokens (bearer).** Local accounts can create their own bearer
tokens through the collection-scoped token endpoints below (or with
`strangle-cli token`, see [configuration](configuration.md#api-tokens)). A
token belongs to exactly one org/collection pair: it authenticates its owner
only on that pair and is rejected everywhere else. Send it as
`Authorization: Bearer st_…` or `X-Strangle-Token: st_…` (the bearer header
wins when both are present). Only the SHA-256 digest is stored; the raw value
is returned exactly once at creation.

**Static hook token.** Independently of UI authentication, when `auth.token`
is set, `POST /api/v1/orgs/{org}/collections/{collection}/hook` accepts
`Authorization: Bearer <token>` or, webhook-friendly,
`X-Strangle-Token: <token>` (the bearer header wins when both are present;
the comparison is constant-time). A valid UI session or user token scoped to
that collection also works on the hook. The static token is **not** a user
account: it is accepted on the hook route only and rejected with `401`
everywhere else when authentication is on. With no users and no static token
the hook is open.

## GET /api/v1/health

Liveness. Open, even when UI authentication is on.

```json
{"status": "ok"}
```

## GET /metrics

Prometheus text exposition (`text/plain; version=0.0.4`), open like
`/api/v1/health` so scrapers need no UI session. It exports build information, Go runtime
numbers and **worker state only** — no run, host or aggregate statistics:

```
# HELP strangle_workers Number of worker slots by state.
# TYPE strangle_workers gauge
strangle_workers{state="total"} 2
strangle_workers{state="alive"} 2
strangle_workers{state="dead"} 0
```

The full set is `strangle_build_info`, `strangle_workers{state=total|alive|dead}`,
`strangle_worker_jobs{worker,collection}`,
`strangle_worker_restarts_total{worker,collection}`,
`process_start_time_seconds` and the common `go_*` runtime gauges. Worker
counts, in-flight jobs and restarts are also shown in the web UI's **Monitor**
tab, which fetches this public endpoint. There is no aggregate stats API: the
dashboard counts hosts and playbooks from the org/collection endpoints.

## POST /api/v1/login

Authenticate a local account and start a session. The session cookie
(`strangle_session`, HttpOnly, SameSite=Lax, 12 h) is set on success.

```json
{"username": "admin", "password": "..."}
```

`200` returns the same body as `GET /api/v1/me`. Errors: `400` invalid JSON,
`401` `invalid username or password` (returned for both an unknown username
and a wrong password, to prevent account enumeration), and `401`
`authentication is not enabled: no users are configured (strangle-cli user add
<name> --password <pw>)` when no account exists.

## POST /api/v1/logout

End the session carried by the request cookie and clear the cookie. Always
`200`:

```json
{"status": "logged out"}
```

## GET /api/v1/me

Answer "who is logged in?" and double as the UI's probe for whether
authentication is on at all:

- `200 {"authenticated": false}` — authentication is off (open server).
- `200 {"authenticated": true, "username": "admin"}` — valid session cookie.
- `401 {"error": "not authenticated"}` — authentication is on and there is no
  valid session (the UI shows its login screen). This is a UI probe, so user
  bearer tokens authenticate the API routes but do not satisfy `/me`.

## Organizations

### GET /api/v1/orgs

List the organizations visible to the caller, each with its collections and
their playbooks. The result merges orgs derived from `collection.yml` with
orgs created through `POST /api/v1/orgs` and stored in the database. An org
created through the API has no collections until a target is configured for
it; it remains visible in the listing.

```json
[
  {"name": "default", "collections": [
    {"name": "homelab", "org": "default", "playbooks": [
      {"name": "site", "playbook": "site.yml", "inventory": "inventory/site.ini"}
    ]}
  ]},
  {"name": "lab", "collections": []}
]
```

A scoped bearer token sees only its own org and collection.

### POST /api/v1/orgs

Create an org, stored in the database. The name must match
`[A-Za-z0-9][A-Za-z0-9_.-]*`. A scoped token cannot create orgs.

```sh
curl -X POST localhost:8080/api/v1/orgs \
  -H "Cookie: strangle_session=..." \
  -d '{"name": "lab"}'
```

`201` returns the new org with an empty collection list:

```json
{"name": "lab", "collections": []}
```

Errors: `400` invalid name or JSON, `401` not authenticated, `403` scoped
token, `409` the org already exists, `503` no database.

### GET /api/v1/orgs/{org}

One org in the same shape as an entry of the list above. `404` for an unknown
org.

### GET /api/v1/orgs/{org}/collections

The collections of one org. `404` for an unknown org.

```json
[{"name": "edge", "org": "lab", "playbooks": []}]
```

### GET /api/v1/orgs/{org}/collections/{collection}

One collection, with its configured playbooks and links to its sub-resources.
Only the surviving collection endpoints are advertised in `links`:

```sh
curl localhost:8080/api/v1/orgs/lab/collections/edge
```

```json
{
  "name": "edge",
  "org": "lab",
  "playbooks": [
    {"name": "site", "playbook": "site.yml", "inventory": "inventory/site.ini"}
  ],
  "links": {
    "hosts":     "/api/v1/orgs/lab/collections/edge/hosts",
    "playbooks": "/api/v1/orgs/lab/collections/edge/playbooks",
    "tokens":    "/api/v1/orgs/lab/collections/edge/tokens",
    "hook":      "/api/v1/orgs/lab/collections/edge/hook"
  }
}
```

## Collection resources

### GET /api/v1/orgs/{org}/collections/{collection}/hosts

Hosts of this collection, with their last status, disappearance state and
latest facts. Hosts are scoped per collection: the same name in two
collections is two entries.

```json
[
  {
    "target": "edge",
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
gets `disappeared_at` set and `active` false until it appears again.
`facts` is the latest `ansible_facts` blob, omitted when none has been
gathered.

### GET /api/v1/orgs/{org}/collections/{collection}/playbooks

Configured jobs of the collection, keyed by collection name, with the latest
run outcome when one exists:

```json
{
  "edge": [
    {"name": "site", "playbook": "site.yml", "inventory": "inventory/site.ini",
     "last_status": "ok", "last_run_at": "..."},
    {"name": "base", "playbook": "base.yml", "inventory": "inventory/base.ini"}
  ]
}
```

`last_status` is `ok` or `failed`; both are omitted for a job that has never
run.

### POST /api/v1/orgs/{org}/collections/{collection}/hook

Git webhook replacing the old `/sync` route. It moves this collection's
checkout to its tracked ref, or to the branch in the request (which becomes
the new tracked ref), and reloads its `strangle.yml`. The applier's view of
the collection's playbooks is refreshed, so a branch switch or a new
`strangle.yml` takes effect from the next run. See
[git-backed collections](git-collections.md).

```sh
curl -X POST localhost:8080/api/v1/orgs/default/collections/site/hook \
  -H "X-Strangle-Token: <token>" \
  -d '{"branch": "release-1.4"}'
```

`{"branch": "..."}` is optional; omitting it syncs to the tracked ref. On
success:

```json
{"target": "site", "rev": "9f2c1ab...", "changed": true}
```

Accepts a UI session, a user bearer token scoped to this collection, or the
static `auth.token` via `Authorization: Bearer <token>` /
`X-Strangle-Token: <token>` (this is the only route where the static token
works). Errors: `401` missing/invalid credentials, `403` token or membership
for another collection, `404` unknown collection or no git-backed collection
configured, `400` invalid JSON or collection without a git upstream, `502`
a git failure (network, auth, broken `strangle.yml`).

### GET /api/v1/orgs/{org}/collections/{collection}/tokens

List the caller's tokens for this exact org/collection pair, ordered by name.
Requires a user identity (session or user token); the static hook token is
rejected. Only a display prefix of the raw value is returned.

```json
[
  {"name": "ci", "prefix": "st_9a1b2c3d", "org": "lab", "collection": "edge",
   "created_at": "2026-11-24T09:12:00Z", "last_used_at": "2026-11-24T10:30:00Z"},
  {"name": "laptop", "prefix": "st_44f0c2aa", "org": "lab", "collection": "edge",
   "created_at": "2026-11-20T08:00:00Z", "expires_at": "2026-11-27T08:00:00Z"}
]
```

`expires_at` is omitted for a token that never expires; `last_used_at` is
omitted until the token has been used. Expired tokens are pruned and not
listed.

### POST /api/v1/orgs/{org}/collections/{collection}/tokens

Create a token scoped to the org/collection in the URL. `ttl_hours` is
optional; `0` or absent means no expiry.

```sh
curl -X POST localhost:8080/api/v1/orgs/lab/collections/edge/tokens \
  -H "Cookie: strangle_session=..." \
  -d '{"name": "ci", "ttl_hours": 72}'
```

`201` returns the raw value — the **only** time it is shown:

```json
{
  "raw": "st_9a1b2c3d...",
  "token": {
    "name": "ci", "prefix": "st_9a1b2c3d", "org": "lab", "collection": "edge",
    "created_at": "..."
  }
}
```

Errors: `400` invalid JSON or empty name, `401`/`403` missing user identity or
access to another collection, `404` unknown collection, `409` a token with
that name already exists for this owner and scope, `503` token storage not
configured. The same scope restriction applies after creation: the token is
rejected on any other org/collection pair.

### DELETE /api/v1/orgs/{org}/collections/{collection}/tokens/{name}

Revoke the named token of this pair. `204` on success, `404` if no such token
exists for the caller in this scope.

## Playbook-scoped host resources

### GET .../playbooks/{playbook}/hosts/{host}/hostkeys

SSH host keys pending approval for one host of one playbook (job name from
`strangle.yml`). Unlike the removed collection-wide host-key list, the
playbook and host are part of the URL. An unknown collection or playbook is
`404`.

```sh
curl localhost:8080/api/v1/orgs/default/collections/site/playbooks/prod/hosts/10.0.0.9/hostkeys
```

```json
[
  {"target": "site/prod", "host": "10.0.0.9", "fingerprint": "SHA256:...",
   "public_key": "ssh-ed25519 AAAA...", "status": "pending", "first_seen": "..."}
]
```

### PATCH .../playbooks/{playbook}/hosts/{host}

Decide a pending key with a JSON Patch array. The only supported operations
are `replace` (also accepted as `add`) on `/status` and `/fingerprint`:

```sh
curl -X PATCH \
  localhost:8080/api/v1/orgs/default/collections/site/playbooks/prod/hosts/10.0.0.9 \
  -H "Content-Type: application/json" \
  -d '[{"op":"replace","path":"/status","value":"approved"},
       {"op":"replace","path":"/fingerprint","value":"SHA256:..."}]'
```

`status` must be `"approved"` or `"rejected"`. The optional `fingerprint`
operation records which key is trusted when approving; reject with
`[{"op":"replace","path":"/status","value":"rejected"}]`. This endpoint
replaces the old `hostkeys/accept` and `hostkeys/reject` routes. On success:

```json
{"status": "approved", "host": "10.0.0.9"}
```

Errors: `400` invalid patch, unsupported path/status, or SSH approval not
enabled; `404` no pending key or unknown collection/playbook; `409`
fingerprint mismatch. These routes apply to jobs using the Go-managed SSH
transport with the `api` host-key policy; see
[the SSH transport](ssh-transport.md).

### GET .../playbooks/{playbook}/hosts/{host}/runs?limit=N

The last runs that included this host in this playbook. `?limit=` defaults to
10; each entry is the run joined with the host's recap line:

```json
[
  {
    "run_id": 1234,
    "target": "site",
    "host": "10.0.0.9",
    "playbook": "prod",
    "started_at": "...",
    "duration_ms": 68500,
    "exit_code": 0,
    "success": true,
    "status": "changed",
    "ok": 12, "changed": 1, "failed": 0, "unreachable": 0,
    "skipped": 0, "rescued": 0, "ignored": 0
  }
]
```

## Removed routes

The following are gone, not renamed aliases: all flat `/api/*` routes; manual
run creation (`POST /api/run` and the collection `/run` route); run listing,
detail, tasks and cancellation; host changes; global hosts, stats, playbooks,
workers and sync endpoints; scoped stats and sync; the collection-wide host-key
list; `ssh/stats`; `hostkeys/accept` and `hostkeys/reject`; and
`GET /api/workers`. Use the hook for git updates, the playbook-scoped host
routes for host keys and host runs, `/metrics` (or the web UI's Monitor tab)
for worker state, and the org/collection endpoints for the dashboard's counts.
