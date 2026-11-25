# API

All endpoints are served by the master process, next to the web UI, on the
address from `server.yml` (`listen_addr`, default `:8080`). Responses are
JSON; errors are `{"error": "<message>"}` with the corresponding status code.

## Authentication

There are two independent mechanisms:

**UI authentication (session cookie).** When at least one local account
exists (see [configuration](configuration.md#ui-authentication)), every
`/api/*` request needs a valid session cookie, except the open endpoints:
`GET /api/health`, `POST /api/login`, `POST /api/logout` and `GET /api/me`.
Sessions live 12 hours and are held in the master's memory (a master
restart logs everyone out). Static UI files are always served; the app
itself gates on `GET /api/me`.

**Sync token (webhooks).** Independently of UI authentication, when
`auth.token` is set `POST /api/sync` accepts
`Authorization: Bearer <token>` or, webhook-friendly,
`X-Strangle-Token: <token>` (the bearer header wins when both are present;
the comparison is constant-time). A valid UI session also works on
`/api/sync`.

## GET /api/health

Liveness. Open, even when UI authentication is on.

```json
{"status": "ok"}
```

## POST /api/login

Authenticate a local account and start a session. The session cookie
(`strangle_session`, HttpOnly, SameSite=Lax, 12 h) is set on success.

```json
{"username": "admin", "password": "..."}
```

`200` returns the same body as `GET /api/me`. Errors: `400` invalid JSON,
`401` `invalid username or password` (returned for both an unknown username
and a wrong password, to prevent account enumeration), and `401`
`authentication is not enabled: no users are configured (strangle user add
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
  "failed_hosts": 0,
  "output": "PLAY [all] ***\n...\n"
}
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
    "last_change_at": "..."
  }
]
```

`last_status` is one of `ok`, `changed`, `failed`, `unreachable` (empty until
the first run). A host that was in scope of a run but missing from the recap
gets `disappeared_at` set; `active` is true while that condition holds.

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

## POST /api/run

Start a playbook run in the background. `playbook` is the **job name** from
`strangle.yml`, not a file path. Unknown targets or job names are rejected
with `404`.

```sh
curl -X POST localhost:8080/api/run \
  -d '{"target": "home", "playbook": "hypervisors"}'
```

```json
{"status": "queued", "target": "home", "playbook": "hypervisors"}
```

`status` is `queued` with a database configured (a River job) or `started`
without (a direct run). The run appears in `/api/runs` when it finishes.

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
`api` host-key policy. The `target` field is the SSH scope key
`<target>/<playbook name>`.

### GET /api/hostkeys

Host keys pending approval, aggregated across all scopes. `status` is
`pending` for an unknown key and `rejected` for a changed key on an approved
host; both are resolved with accept or reject.

```json
[
  {"target": "site/prod", "host": "10.0.0.9",
   "fingerprint": "SHA256:...", "public_key": "ssh-ed25519 AAAA...",
   "status": "pending", "first_seen": "..."}
]
```

### POST /api/hostkeys/accept

```json
{"target": "site/prod", "host": "10.0.0.9", "fingerprint": "SHA256:..."}
```

Approves the pending key (the fingerprint must match a pending one) and
persists it for future connections.

```json
{"status": "accepted", "host": "10.0.0.9"}
```

### POST /api/hostkeys/reject

```json
{"target": "site/prod", "host": "10.0.0.9"}
```

Removes the pending key; the connection stays refused until re-approved.

```json
{"status": "rejected", "host": "10.0.0.9"}
```

### GET /api/ssh/stats

Counters of the master's SSH managers, keyed by scope:

```json
{
  "site/prod": {"active_connections": 0, "total_dials": 0},
  "home/hypervisors": {"active_connections": 0, "total_dials": 0}
}
```

The managers of the process serving this API exist for the host-key approval
endpoints; the playbook's actual SSH connections are pooled inside the worker
process that runs it. The endpoint is mainly useful to see which scopes are
configured (one entry per job with the transport enabled).
