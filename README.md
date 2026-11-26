# strangle

[![CI](https://github.com/kakwa/strangle/actions/workflows/ci.yml/badge.svg)](https://github.com/kakwa/strangle/actions/workflows/ci.yml)

Strangle applies Ansible playbooks on a schedule and reports on the results
through an HTTP API and web UI. Playbooks are run **inside Go worker
processes** through an embedded CPython interpreter (via
[`go.nhat.io/cpy/v3`](https://pkg.go.dev/go.nhat.io/cpy/v3)): no
`ansible-playbook` process is spawned.

Every run is recorded: run metadata, a per-host recap (ok / changed / failed /
unreachable) and every per-host × per-task result with its status, diff and
error. A separate latest-state table keeps the last status per host/task
internally, but it is not exposed through the API. The dashboard shows simple
counts computed from the org/collection, host and playbook listings; there is
no aggregate stats endpoint.

## How it works

The server binary (`strangle-server serve`) owns the configuration, the HTTP API
and the schedule. It spawns a pool of worker child processes
(`strangle-server worker`), each owning one embedded Ansible interpreter, and
dispatches playbook runs to them over unix sockets using a small JSON-RPC
protocol in `controller/worker` (no code generation).
A master supervises the workers: health pings and automatic restarts with
backoff. Worker state is observable through the public Prometheus `/metrics`
endpoint and the web UI's Monitor tab.

Each (target, playbook) job has its own robfig/cron v3 schedule (or inherits
the server fallback), and the scheduler enqueues it when due. PostgreSQL is
mandatory: run history, accounts, tokens, collection memberships and the
[River](https://github.com/riverqueue/river) job queue all live in the same
database. A River client in the master consumes playbook jobs with at most as
many concurrent runs as there are workers. River owns only queue metadata;
the applier records every result through the store in the run-history tables.

Git-backed targets are cloned into their configured directory and pinned to a
tracked ref (branch, tag or commit); the collection `hook` endpoint is a
webhook that re-syncs a target — optionally switching the tracked branch —
and reloads its `strangle.yml`, so per-branch content takes effect immediately.

The details: [architecture](api/docs/architecture.md), [configuration reference](api/docs/configuration.md),
[API reference](api/docs/api.md), [git-backed collections](api/docs/git-collections.md),
[the Go-managed SSH transport](api/docs/ssh-transport.md), including short-lived
user certificates signed by a local CA or by Vault through AppRole.

## Layout

```
cmd/strangle-server/ server entrypoint: config, wiring, worker subcommand, graceful shutdown
cmd/strangle-cli/    client entrypoint: API client and account/token/access commands
api/                 HTTP API: Echo transport, auth middleware, routes, /metrics,
                     OpenAPI docs and the built-in UI
  docs/              documentation (architecture, configuration, API) and the
                     generated OpenAPI 3.1 package (swaggo)
controller/          HTTP handlers and swaggo annotations
  applier/           embedded CPython runner, output parser and result recording
  maintenance/       periodic run retention and stale-host cleanup
  scheduler/         runs each job on its own schedule (live job list)
  worker/            worker pool, JSON-RPC transport and the worker service
    riverqueue/      postgres-backed River job queue for playbook runs
model/               GORM/PostgreSQL store, run records and shared value types
collection/          git-backed targets: clone, pin to tracked ref, sync, reload
security/
  auth/              local accounts, sessions, scoped tokens and collection access
  sandbox/           Landlock sandboxing for worker child processes
utils/
  cfg/               YAML config: server.yml, collection.yml, per-target strangle.yml
  git/               in-process git operations (go-git), no system git binary
  logger/            zap logger construction
  sshpool/           Go-managed persistent SSH connections with host-key policies
tests/
  integration_test/  DB, API, queue and Ansible suites (external test packages)
  testutil/          shared test helpers (store setup, JSON fixtures)
  config/
    strangle.yml         sample per-target configuration (reference)
  demo/                self-contained demo target (applies a playbook to
                       localhost); add it to collection.yml to run it
    strangle.yml           per-target configuration
    playbook.yml           the demo playbook
    inventory.ini          the demo inventory
  ssh/                   Go-managed SSH transport example
    playbook.yml             the SSH demo playbook
    inventory.ini            inventory pointing at a remote host
  sandbox/validate/      Landlock isolation validation collection
    README.md                setup and checks
    strangle.yml             per-target configuration
    validate.yml             the validation playbook
    inventory.ini            local inventory
    secret.txt               the collection's own secret
ui/                  React (Vite) web UI, styled with Mantine; built assets are
                     embedded into the binary (go:embed)
config/              base configuration, loaded by default
  server.yml           process-level settings (listen address, schedule, DSN)
  collection.yml       the list of Ansible projects (targets) to apply
```

## What is stored

- **Run** — one playbook invocation: target, playbook, start/finish,
  duration, exit code, host/changed/failed counts, full captured output.
- **HostRun** — per-run, per-host recap line (`ok`, `changed`, `failed`,
  `unreachable`, `skipped`, `rescued`, `ignored`).
- **TaskResult** — append-only per-run history of each host × task result,
  including its status, diff and error.
- **TaskState** — per host × task (play + task name): the last status, when
  it last ran, the last diff and the last error. Updated on every run.
- **Host** — every host (identified by target + name, so the same name in
  two targets is two hosts) seen in an inventory or recap; `active` tracks
  whether it was in scope of a run but missing from the recap
  ("disappeared"). `facts` is the latest `ansible_facts` jsonb snapshot,
  refreshed only when a run returns facts.

## API (summary)

Everything is under `/api/v1`; the old flat `/api/*` routes were removed.

- `GET /api/v1/health`   — liveness (open, no auth)
- `GET /metrics`         — Prometheus text exposition: build info, Go runtime and worker state (open)
- `GET /api/v1/openapi.json` — generated OpenAPI 3.1 document (open)
- `GET /api/docs`        — Swagger UI (open), with assets under `/api/docs/`
- `POST /api/v1/login`   — authenticate a local account, sets a 12 h session cookie
- `POST /api/v1/logout`  — end the session
- `GET /api/v1/me`       — who is logged in; the UI's auth probe
- `GET /api/v1/orgs`     — orgs and their collections, merging configured orgs
  with orgs created through the API and stored in the database
- `POST /api/v1/orgs`    — body `{"name": "..."}`; create an org (201, duplicate 409)
- `GET /api/v1/orgs/{org}` — one org
- `GET /api/v1/orgs/{org}/collections` — the collections of one org
- `GET /api/v1/orgs/{org}/collections/{collection}` — one collection, with
  links to `hosts`, `playbooks`, `tokens` and `hook`
- `GET /api/v1/orgs/{org}/collections/{collection}/hosts` — hosts with last
  status, disappearance state and latest facts
- `GET /api/v1/orgs/{org}/collections/{collection}/playbooks` — jobs keyed by
  collection, with the last outcome
- `POST /api/v1/orgs/{org}/collections/{collection}/hook` — body
  `{"branch": "..."}` (optional); re-syncs a git-backed collection,
  optionally switching the tracked branch. Accepts a UI session, a user token
  scoped to that collection, or the static `auth.token` (hook only)
- `GET|POST /api/v1/orgs/{org}/collections/{collection}/tokens` — list/create
  tokens scoped to one org/collection; create body
  `{"name": "...", "ttl_hours": N}` returns the raw value exactly once
- `DELETE /api/v1/orgs/{org}/collections/{collection}/tokens/{name}` — revoke one
- `GET /api/v1/orgs/{org}/collections/{collection}/playbooks/{playbook}/hosts/{host}/hostkeys`
  — SSH host keys pending approval for one host of one playbook
- `PATCH /api/v1/orgs/{org}/collections/{collection}/playbooks/{playbook}/hosts/{host}`
  — JSON Patch array, e.g.
  `[{"op":"replace","path":"/status","value":"approved"}]` (or `rejected`),
  with an optional `/fingerprint` replacement
- `GET /api/v1/orgs/{org}/collections/{collection}/playbooks/{playbook}/hosts/{host}/runs?limit=N`
  — the last runs of that host in that playbook (default 10)

Full request/response examples: [api/docs/api.md](api/docs/api.md), or the served
Swagger UI at `/api/docs` backed by `/api/v1/openapi.json`. The server routes
requests with Echo, and the document is generated from swaggo/swag v2
annotations by `make swag`.

## Web UI

Single-page app (React + Mantine) with bookmarkable routes, polled every 10
seconds. When UI authentication is on (see below), an unauthenticated visit
shows a login screen; the header shows the signed-in user and a log-out
button:

- `/` — **Dashboard**: simple counts of hosts, playbooks and collections,
  derived from the org/collection, host and playbook listings (no stats API).
- `/hosts` — **Hosts**: the collection's host table with last status and
  disappearance state, with a name filter.
- `/hosts/{host}` — **Host detail**: the latest Ansible facts and the last
  runs for that host in a selected playbook.
- `/playbooks` — **Playbooks**: the collection's jobs with their last outcome.
- `/tokens` — **Tokens**: the signed-in user's tokens for the selected
  org/collection — create one (the raw value is shown once), see its prefix,
  scope, lifetime and last use, revoke it.
- `/ssh` — **SSH**: pending SSH host keys of a selected playbook and host,
  with approve/reject actions.
- `/monitor` — **Monitor**: worker pool state fetched from the public
  Prometheus `/metrics` endpoint (total/alive/dead, jobs and restarts per
  worker).

## Configuration

Configuration is file-only: three kinds of YAML files, located with the `-s`
and `-c` flags (defaults `./config/server.yml` and `./config/collection.yml`).
There are no environment variables.

```sh
strangle-server serve [-s config/server.yml] [-c config/collection.yml]
```

### server.yml — process-level settings

| Key             | Default         | Description                          |
|-----------------|-----------------|--------------------------------------|
| `listen_addr`   | `:8080`         | HTTP listen address                  |
| `log_level`     | `info`          | `debug`, `info`, `warn` or `error`   |
| `schedule`      | `@every 15m`    | Fallback robfig/cron v3 schedule for jobs without their own `schedule` in `strangle.yml` |
| `workers`       | `1`             | Worker child processes (one embedded interpreter each) |
| `database.dsn`  | —               | PostgreSQL DSN; required for history, accounts and the job queue |
| `auth.token`    | —               | Bearer token for the collection `hook` webhook only, independent of UI authentication |
| `auth.cli_token` | —               | User API token (`st_…`) used by the CLI subcommands; override with `--token` |
| `sandbox.enabled` | `false`       | Landlock isolation: one worker set per collection, read-only checkouts, private write scratch |
| `retention.max_age` / `retention.max_count` | — | Purge finished runs older than the duration and/or beyond the newest N per collection; `retention.interval` defaults to `1h` |
| `host_cleanup.delay` | — | Remove hosts absent from a collection's inventory and unseen for this duration; `host_cleanup.interval` defaults to `1h` |
| `tls.cert_file` / `tls.key_file` | — | Serve the API and UI over HTTPS (both required; otherwise plain HTTP) |

### Collection sandboxing

With `sandbox.enabled: true`, Landlock confines each worker to one collection:
playbooks cannot read another collection's credentials or modify their own
checkout. Ansible writes are redirected to a per-worker scratch directory.
A [validating collection](tests/sandbox/validate/README.md) is provided to verify
the isolation. See [configuration](api/docs/configuration.md#collection-sandboxing).

### UI authentication

No flag: UI authentication is **on as soon as at least one local account
exists** (accounts live in the configured database, so a DSN is required).
While on, every `/api/v1` endpoint needs a valid 12-hour session cookie,
except health, login, logout, me, the OpenAPI document and the Swagger UI.
Manage accounts:

```sh
strangle-cli user add <name> --password-stdin
strangle-cli user list
strangle-cli user remove <name>
```

#### API tokens

Local accounts can create bearer **API tokens** that authenticate as that
account on one org/collection pair only — for CLIs, webhooks and other
integrations. A token used on any other pair is rejected. Tokens are managed
in the web UI under *Tokens* or from the command line:

```sh
strangle-cli token create ci --user admin --org lab --collection edge --ttl 72h  # prints the raw value once
strangle-cli token list [--user admin] [--org lab] [--collection edge]
strangle-cli token remove ci --user admin [--org lab] [--collection edge]
```

`token create` requires `--user`, `--org` and `--collection`. `list` and
`remove` accept optional `--org`/`--collection` filters; omitting a filter
matches all orgs/collections. Only the SHA-256 digest of a token is stored;
the raw value is shown exactly once, at creation. A token is sent as
`Authorization: Bearer st_…` or `X-Strangle-Token: st_…`. The static
`auth.token` above is a separate mechanism: it only opens the collection
`hook` route and is not a user account.

#### CLI client

The `strangle-cli` binary holds the client-side commands. Its API-backed
subcommands use the only API layout, under
`/api/v1/orgs/{org}/collections/{collection}`. The org comes from
collection.yml (default `default`), or from `--org`. There are no `run` or
`runs` commands: runs are scheduled, and recent runs are read through a host:

```sh
strangle-cli hosts [target]
strangle-cli host <name> <target> --playbook NAME [--limit N]
strangle-cli hostkeys list <target> --playbook NAME [host]
strangle-cli hostkeys approve <target> <host> --playbook NAME [--fingerprint SHA256:...]
strangle-cli hostkeys reject <target> <host> --playbook NAME
```

`hostkeys approve` and `reject` also accept the older `<target>/<playbook>`
spelling instead of a bare target plus `--playbook`; the fingerprint can be
given as a third argument instead of `--fingerprint`.

All of them accept `-s/--server-config`, `-c/--collection-config`, `--url`,
`--token` and `--json`. Without `--url`, the API address is derived from
`listen_addr` (and switches to HTTPS when `tls` is configured); without
`--token`, the user token comes from `auth.cli_token` in server.yml. Output is
tab-separated by default and JSON with `--json`. On an open server no token is
needed.

#### Per-collection access

The API is versioned (`/api/v1`) and groups collections into orgs:
`/api/v1/orgs/{org}/collections/{collection}`; there are no flat aliases.
An org comes from the optional `org` field of a collection.yml entry
(default: `default`), or can be created through `POST /api/v1/orgs`. The org
listing merges configured orgs with the ones stored in the database. Once a
membership exists, users only see and can use their own collections; a bearer
token is further limited to its own org/collection pair:

```sh
strangle-cli collection grant alice lab edge
strangle-cli collection revoke alice lab edge
strangle-cli collection list [--user alice]
```

### collection.yml — the targets

The inventory of Ansible projects (targets) that strangle applies. Every
target is a directory containing a `strangle.yml`; a `git` block makes the
target git-backed. Relative `path` values resolve against this file's
directory:

```yaml
collections:
  - name: home
    path: /path/to/ansible        # the project directory (contains strangle.yml)
    # git:                        # optional: upstream repository
    #   url: https://example.com/repo.git
    #   branch: main              # tracked ref (branch, tag or commit)
    # retention:                  # optional: overrides server.yml limits
    #   max_age: 720h
    #   max_count: 1000
    # host_cleanup:               # optional: overrides server.yml delay
    #   delay: 720h
```

### strangle.yml — one per target

Lives at the root of the target directory. Each entry of `playbooks` is a
named job with its own playbook file, inventory, schedule and options:

```yaml
schedule: "*/5 * * * *"        # optional target-level default
playbooks:
  - name: hypervisors
    playbook: hypervisor.yml
    inventory: inventory
    schedule: "0 3 * * 1"      # cron: Mondays at 03:00
  - name: base_infra
    playbook: infra.yml
    inventory: inventory
    schedule: "@every 30m"     # cron descriptor
```

In git-backed targets, `working_dir`, `playbook`, `inventory` and `schedule`
may use `{{ .Branch }}` and `{{ .Name }}` templates, e.g.
`inventory: inventories/{{ .Branch }}.ini`.

The full reference — schedule syntax, every option, the Go-managed SSH
transport and its host-key policies — is in
[api/docs/configuration.md](api/docs/configuration.md).

## Running

Requires Go (with cgo enabled), Node.js, `pkg-config`, Python 3 development
headers and Ansible importable from that Python. The `make` targets configure
`PKG_CONFIG` to map cpy/v3's `python-3.12-embed` lookup onto the installed
`python3-embed` package; use them rather than plain `go build`/`go test` (or
set `PKG_CONFIG` yourself).

The web UI is built with Vite and **embedded into the server binary**
(`go:embed`), so `bin/strangle-server` is self-contained — no `ui/dist` needed
at runtime. `make build` rebuilds the UI before compiling both binaries.

```sh
# build the UI (into ui/dist), then the server and client binaries
make build
./bin/strangle-server serve         # reads ./config/server.yml and ./config/collection.yml
./bin/strangle-cli hosts demo       # talks to the server through its API

# or point at explicit config files
./bin/strangle-server serve -s config/server.yml -c config/collection.yml

# web UI (development): Vite dev server on :5173, proxies /api to :8080
make ui-dev

# web UI (production): build into ui/dist only
make ui
```

### Shutdown

`SIGINT` and `SIGTERM` trigger a graceful shutdown. The HTTP server stops
accepting connections, no further playbooks are started, the job queue drains
(any run that is already in flight is allowed to **finish and be persisted**
inside its worker) and only then are the workers terminated and the store
closed. Send `SIGKILL` to abort a run immediately.

Internally, Ansible's `TaskQueueManager` replaces the process-wide SIGINT/
SIGTERM handlers with its own during a run, which would clobber the C-level
handlers Go's `os/signal` installs. The embedded runner refuses those two
`signal.signal` calls (a guard installed before Ansible is imported) so signal
delivery keeps working for the life of the process.

## Status

Playbook scheduling, worker supervision, PostgreSQL-backed history, the
postgres-backed job queue, git-backed collections, per-task change traces and
the web UI are implemented and exercised by the test suite (PostgreSQL tests
run when `STRANGLE_TEST_DSN` is set).

## License

Strangle is free software, licensed under the GNU General Public License,
version 3 or (at your option) any later version. See [LICENSE](LICENSE) for
the full text.

Copyright (c) 2026 Carpentier Pierre-Francois.
