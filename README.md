# strangle

[![CI](https://github.com/kakwa/strangle/actions/workflows/ci.yml/badge.svg)](https://github.com/kakwa/strangle/actions/workflows/ci.yml)

Strangle applies Ansible playbooks on a schedule and reports on the results
through an HTTP API and web UI. Playbooks are run **inside Go worker
processes** through an embedded CPython interpreter (via
[`go.nhat.io/cpy/v3`](https://pkg.go.dev/go.nhat.io/cpy/v3)): no
`ansible-playbook` process is spawned.

Every run is recorded: run metadata, a per-host recap (ok / changed / failed /
unreachable) and every per-host × per-task result with its status, diff and
error. A separate latest-state view keeps the familiar host task traces.
Fleet statistics — host count, hosts that disappeared from the recap, run
duration, change/failed counts — are derived from the stored history.

## How it works

The master process (`strangle serve`) owns the configuration, the HTTP API and the
schedule. It spawns a pool of worker child processes (`strangle worker`), each
owning one embedded Ansible interpreter, and dispatches playbook runs to them
over unix sockets (a small gRPC service, `proto/strangle/v1/runner.proto`).
A master supervises the workers: health pings, automatic restarts with
backoff, and `GET /api/workers` for observability.

Each (target, playbook) job has its own schedule — a duration, a cron
expression, or the global fallback interval — and the scheduler enqueues it
when due. With a PostgreSQL DSN configured, runs flow through a
postgres-backed [River](https://github.com/riverqueue/river) job queue and a
River client in the master consumes them with at most as many concurrent runs
as there are workers. Without a DSN, an in-memory store is used and runs
execute directly. Failed runs can notify webhook, Slack or email endpoints
configured per collection.

Git-backed targets are cloned into their configured directory and pinned to a
tracked ref (branch, tag or commit); `POST /api/sync` is a webhook that
re-syncs a target — optionally switching the tracked branch — and reloads its
`strangle.yml`, so per-branch content takes effect immediately.

The details: [architecture](docs/architecture.md), [configuration reference](docs/configuration.md),
[API reference](docs/api.md), [git-backed collections](docs/git-collections.md),
[the Go-managed SSH transport](docs/ssh-transport.md), including short-lived
user certificates signed by a local CA or by Vault through AppRole.

## Layout

```
cmd/strangle/        entrypoint: config, wiring, worker subcommand, graceful shutdown
internal/
  ansible/           embedded CPython runner + Go-backed Ansible connection plugin
  config/            YAML config: server.yml, collection.yml, per-target strangle.yml
  applier/           dispatches each run to a worker, parses output, records results
  collection/        git-backed targets: clone, pin to tracked ref, sync, reload
  git/               in-process git operations (go-git), no system git binary
  inventory/         parses YAML inventory files into host groups and members
  notify/            failure notifications (webhook, Slack, email)
  parser/            parses ansible-playbook output (recap, task results, diffs)
  schedule/          parses durations, cron expressions and aliases
  scheduler/         runs each job on its own schedule (live job list)
  riverqueue/        postgres-backed River job queue for playbook runs
  server/            HTTP API + serves the built web UI
  sshpool/           Go-managed persistent SSH connections with host-key policies
  store/             run records and the Store interface (in-memory + PostgreSQL)
  db/                GORM/PostgreSQL implementation of the Store
  worker/            worker pool: spawning, gRPC client, supervision and restarts
proto/               the gRPC Runner service between master and workers
web/                 React (Vite) web UI, styled with Mantine; built assets are
                     embedded into the binary (go:embed)
config/              base configuration, loaded by default
  server.yml           process-level settings (listen address, interval, DSN)
  collection.yml       the list of Ansible projects (targets) to apply
  strangle.yml         sample per-target configuration (reference)
demo/                self-contained demo target (applies a playbook to
                     localhost); add it to collection.yml to run it
  strangle.yml           per-target configuration
  playbook.yml           the demo playbook
  inventory.ini          the demo inventory
tests/               test configurations
  ssh/                   Go-managed SSH transport example
    playbook.yml             the SSH demo playbook
    inventory.ini            inventory pointing at a remote host
docs/                documentation: architecture, configuration, API, git collections
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

- `GET /api/health`      — liveness (open, no auth)
- `GET /metrics`         — Prometheus text exposition: aggregate run, host and worker stats (open)
- `GET /api/v1/openapi.json` — generated OpenAPI 3 document (open)
- `GET /api/docs`        — Swagger UI for the versioned API (open)
- `POST /api/login`      — authenticate a local account, sets a 12 h session cookie
- `POST /api/logout`     — end the session
- `GET /api/me`          — who is logged in; the UI's auth probe
- `GET /api/stats`       — fleet statistics (hosts, active, disappeared, run counts, durations)
- `GET /api/runs`        — runs, newest first (`?limit=`)
- `GET /api/hosts`       — all hosts with their last status, disappearance state and latest facts
- `GET /api/hosts/{host}/runs`    — the last runs that included the host (default 10)
- `GET /api/hosts/{host}/changes` — the host's task traces, newest first (default 200)
- `GET /api/playbooks`   — playbook job names, keyed by target name, with last outcome
- `GET /api/workers`     — the worker pool: pid, socket, liveness, jobs, restarts
- `POST /api/v1/orgs/{org}/collections/{collection}/run` — body
  `{"target": "demo", "playbook": "demo", "check": false}`; starts the run in
  the background and returns `202`. `"check": true` is a dry run (`--check`).
  The flat `POST /api/run` remains as a legacy alias
- `POST /api/sync`       — body `{"target": "...", "branch": "..."}`; re-syncs a
  git-backed target, optionally switching the tracked branch. Accepts a UI
  session, a user token, or the configured token when `auth.token` is set
- `GET /api/tokens`      — list the caller's API tokens (prefix only, never the raw value)
- `POST /api/tokens`     — body `{"name": "...", "ttl_hours": N}`; create one,
  returns the raw value exactly once
- `DELETE /api/tokens/{name}` — revoke one
- `GET /api/v1/orgs/{org}/collections/{collection}/hostkeys` — SSH host keys
  pending approval (collection-scoped, like all collection data)
- `POST /api/v1/orgs/{org}/collections/{collection}/hostkeys/accept` /
  `POST /api/v1/orgs/{org}/collections/{collection}/hostkeys/reject`
- `GET /api/v1/orgs/{org}/collections/{collection}/ssh/stats` — SSH pool
  counters per scope
- `GET /api/v1/orgs/{org}/collections/{collection}/runs/{id}/tasks` — every
  per-task result of one run
- `POST /api/v1/orgs/{org}/collections/{collection}/runs/{id}/cancel` — cancel
  a queued or running run; the run detail view offers the same action

Full request/response examples: [docs/api.md](docs/api.md), or the served
Swagger UI at `/api/docs` backed by `/api/v1/openapi.json`.

## Web UI

Single-page app (React + Mantine) with bookmarkable routes, polled every 10
seconds. When UI authentication is on (see below), an unauthenticated visit
shows a login screen; the header shows the signed-in user and a log-out
button:

- `/` — **Dashboard**: fleet stats and the most recent runs.
- `/runs/{id}` — **Run detail**: metadata, the full captured output and the
  run's per-task results (with expandable diffs and errors), fetched only
  when the run is opened.
- `/hosts` — **Hosts**: the host table with last status and disappearance state.
- `/hosts/{host}` — **Host detail**: the latest Ansible facts, the last runs
  for that host, and its task traces with expandable diffs and errors.
- `/playbooks` — **Playbooks**: every target's jobs with their last outcome,
  plus a manual-run button per job.
- `/tokens` — **Tokens**: the signed-in user's API tokens — create one (the
  raw value is shown once), see its prefix, lifetime and last use, revoke it.
- `/ssh` — **SSH**: pending SSH host keys, with approve/reject actions, and
  the master's SSH connection-pool counters.

## Configuration

Configuration is file-only: three kinds of YAML files, located with the `-s`
and `-c` flags (defaults `./config/server.yml` and `./config/collection.yml`).
There are no environment variables.

```sh
strangle serve [-s config/server.yml] [-c config/collection.yml]
```

### server.yml — process-level settings

| Key             | Default         | Description                          |
|-----------------|-----------------|--------------------------------------|
| `listen_addr`   | `:8080`         | HTTP listen address                  |
| `log_level`     | `info`          | `debug`, `info`, `warn` or `error`   |
| `interval`      | `15m`           | Fallback for jobs without their own `schedule`/`interval` in `strangle.yml` |
| `workers`       | `1`             | Worker child processes (one embedded interpreter each) |
| `database.dsn`  | —               | PostgreSQL DSN; without a DSN an in-memory store is used |
| `auth.token`    | —               | Bearer token for `POST /api/sync` (webhook), independent of UI authentication |
| `sandbox.enabled` | `false`       | Landlock isolation: one worker set per collection, read-only checkouts, private write scratch |
| `retention.max_age` / `retention.max_count` | — | Purge finished runs older than the duration and/or beyond the newest N per collection; `retention.interval` defaults to `1h` |
| `host_cleanup.delay` | — | Remove hosts absent from a collection's inventory and unseen for this duration; `host_cleanup.interval` defaults to `1h` |
| `tls.cert_file` / `tls.key_file` | — | Serve the API and UI over HTTPS (both required; otherwise plain HTTP) |

### Collection sandboxing

With `sandbox.enabled: true`, Landlock confines each worker to one collection:
playbooks cannot read another collection's credentials or modify their own
checkout. Ansible writes are redirected to a per-worker scratch directory.
A [validating collection](sandbox/validate/README.md) is provided to verify
the isolation. See [configuration](docs/configuration.md#collection-sandboxing).

### UI authentication

No flag: UI authentication is **on as soon as at least one local account
exists** (accounts live in the configured database, so a DSN is required).
While on, every `/api/*` endpoint needs a valid 12-hour session cookie,
except health, login, logout and me. Manage accounts:

```sh
strangle user add <name> --password-stdin
strangle user list
strangle user remove <name>
```

#### API tokens

Local accounts can create bearer **API tokens** that authenticate any
`/api/*` request as that account — for CLIs, webhooks and other integrations.
Tokens are managed in the web UI under *Tokens* or from the command line:

```sh
strangle token create ci --user admin --ttl 72h   # prints the raw value once
strangle token list [--user admin]
strangle token remove ci --user admin
```

Only the SHA-256 digest of a token is stored; the raw value is shown exactly
once, at creation. A token is sent as `Authorization: Bearer st_…` or
`X-Strangle-Token: st_…`. The static `auth.token` above is a separate
mechanism: it only opens `POST /api/sync` and is not a user account.

#### Per-collection access

The canonical API is versioned (`/api/v1`) and groups collections into orgs:
`/api/v1/orgs/{org}/collections/{collection}`. The old flat routes remain as
aliases. An org comes from the optional `org` field of a collection.yml entry
(default: `default`). Once a membership exists, users only see and can use
their own collections:

```sh
strangle collection grant alice lab edge
strangle collection revoke alice lab edge
strangle collection list [--user alice]
```

### collection.yml — the targets

The inventory of Ansible projects (targets) that strangle applies. Every
target is a directory containing a `strangle.yml`; a `git` block makes the
target git-backed. Relative `path` values resolve against this file's
directory:

```yaml
targets:
  - name: home
    path: /path/to/ansible        # the project directory (contains strangle.yml)
    # git:                        # optional: upstream repository
    #   url: https://example.com/repo.git
    #   branch: main              # tracked ref (branch, tag or commit)
    # notifications:              # optional: sent when a run fails
    #   - type: webhook
    #     url: https://example.com/strangle-hook
    #     headers:
    #       Authorization: Bearer secret
    #   - type: slack
    #     url: https://hooks.slack.com/services/T/B/X
    #   - type: email
    #     from: strangle@example.com
    #     to: [ops@example.com]
    #     smtp: smtp.example.com:587
    #     username: strangle
    #     password_file: /run/secrets/smtp
```

Notifiers are configured per collection (here), never in a git-backed
`strangle.yml`. They run in parallel with a 10-second default timeout and
include a bounded tail of the failed playbook's output. See
[docs/configuration.md](docs/configuration.md#failure-notifications) for all
fields.

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
    interval: 30m              # duration shorthand
```

In git-backed targets, `working_dir`, `playbook`, `inventory`, `schedule` and
`interval` may use `{{ .Branch }}` and `{{ .Name }}` templates, e.g.
`inventory: inventories/{{ .Branch }}.ini`.

The full reference — schedule syntax, every option, the Go-managed SSH
transport and its host-key policies — is in
[docs/configuration.md](docs/configuration.md).

## Running

Requires Go (with cgo enabled), Node.js, `pkg-config`, Python 3 development
headers and Ansible importable from that Python. The `make` targets configure
`PKG_CONFIG` to map cpy/v3's `python-3.12-embed` lookup onto the installed
`python3-embed` package; use them rather than plain `go build`/`go test` (or
set `PKG_CONFIG` yourself).

The web UI is built with Vite and **embedded into the binary** (`go:embed`),
so `bin/strangle` is self-contained — no `web/dist` needed at runtime.
`make build` rebuilds the UI before compiling.

```sh
# build the UI (into web/dist) and the server, UI embedded
make build
./bin/strangle serve                # reads ./config/server.yml and ./config/collection.yml

# or point at explicit config files
./bin/strangle serve -s config/server.yml -c config/collection.yml

# web UI (development): Vite dev server on :5173, proxies /api to :8080
make web-dev

# web UI (production): build into web/dist only
make web
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
