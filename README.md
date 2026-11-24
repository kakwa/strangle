# strangle

[![CI](https://github.com/kakwa/strangle/actions/workflows/ci.yml/badge.svg)](https://github.com/kakwa/strangle/actions/workflows/ci.yml)

Strangle applies Ansible playbooks on a schedule and reports on the results
through an HTTP API and web UI. Playbooks are run **inside the Go process**
through an embedded CPython interpreter (via
[`go.nhat.io/cpy/v3`](https://pkg.go.dev/go.nhat.io/cpy/v3)): no
`ansible-playbook` process is spawned.

Every run is recorded: run metadata, a per-host recap (ok / changed / failed /
unreachable) and a per-host × per-task trace of the last status, diff and
error. Fleet statistics — host count, hosts that disappeared from the recap,
run duration, change/failed counts — are derived from the stored history.

## Layout

```
cmd/strangle/        entrypoint: config, wiring, graceful shutdown
internal/
  ansible/           embedded CPython runner + Go-backed Ansible connection plugin
  config/            YAML config file + environment variables
  applier/           runs the playbook list, parses output, records results
  parser/            parses ansible-playbook output (recap, task results, diffs)
  inventory/         parses YAML inventory files into host groups and members
  scheduler/         runs a cycle on an interval
  store/             run records and the Store interface (in-memory + PostgreSQL)
  db/                GORM/PostgreSQL implementation of the Store
  server/            HTTP API + serves the built web UI
  sshpool/           Go-managed persistent SSH connections with host-key policies
web/                 React (Vite) web UI, styled with Mantine
strangle.yaml        base configuration
strangle-remote.yaml example using the Go-managed SSH transport
```

## What is stored

- **Run** — one `ansible-playbook` invocation: playbook, start/finish,
  duration, exit code, host/changed/failed counts, full captured output.
- **HostRun** — per-run, per-host recap line (`ok`, `changed`, `failed`,
  `unreachable`, `skipped`, `rescued`, `ignored`).
- **TaskState** — per host × task (play + task name): the last status, when
  it last ran, the last diff and the last error. Updated on every run.
- **Host** — every host seen in an inventory or recap; `active` tracks
  whether it was in scope of a run but missing from the recap
  ("disappeared").

## API

- `GET /api/health`      — liveness
- `GET /api/stats`       — fleet statistics (hosts, active, disappeared, run counts, durations)
- `GET /api/runs`        — runs, newest first (`?limit=`)
- `GET /api/hosts`       — all hosts with their last status and disappearance state
- `GET /api/hosts/{host}/runs`    — the last runs that included the host (default 10)
- `GET /api/hosts/{host}/changes` — the host's task traces, newest first (default 200)
- `GET /api/playbooks`   — configured playbooks
- `POST /api/run`        — body `{"playbook": "site.yml"}`; starts the run in
  the background and returns `202` (unknown playbook names are rejected)
- `GET /api/hostkeys`    — host keys pending approval (SSH transport only)
- `POST /api/hostkeys/accept` — body `{"host": "...", "fingerprint": "..."}`
- `POST /api/hostkeys/reject` — body `{"host": "..."}`
- `GET /api/ssh/stats`   — SSH pool counters (`active_connections`, `total_dials`)

## Web UI

Single page (React + Mantine) with three views, polled every 10 seconds:

- **Dashboard** — fleet stats and the most recent runs.
- **Hosts** — the host table with last status and disappearance state.
- **Host detail** — the last 10 runs for that host, plus its task traces with
  expandable diffs and errors.

## Configuration

Runtime settings read from the environment:

| Variable            | Default         | Description                          |
|---------------------|-----------------|--------------------------------------|
| `STRANGLE_CONFIG`   | `strangle.yaml` | Path to the YAML config file         |
| `STRANGLE_ADDR`     | `:8080`         | HTTP listen address                  |
| `STRANGLE_INTERVAL` | `15m`           | How often the playbook list is run   |
| `STRANGLE_DSN`      | —               | PostgreSQL DSN; overrides `database.dsn`; without a DSN an in-memory store is used |

The YAML file holds the Ansible parameters:

```yaml
working_dir: /path/to/ansible   # the Ansible project root (optional)
inventory: inventory            # inventory file or directory, relative to working_dir
playbooks:                      # ordered list, relative to working_dir
  - site.yml
  - dns.yml

# Legacy single-playbook form (equivalent to playbooks: [playbook.yml]):
# playbook: playbook.yml

user: deploy             # optional: -u
become: true             # optional: -b
become_method: sudo      # optional: --become-method (default: sudo)
become_user: root        # optional: --become-user

# Password files instead of interactive prompts (-K/-k cannot be answered in-process):
become_password_file: /run/secrets/ansible_become
connection_password_file: /run/secrets/ansible_connection
vault_password_file: /run/secrets/ansible_vault

database:
  dsn: postgres://user:pass@127.0.0.1:5432/strangle?sslmode=disable
```

`--diff` is always added by the applier. `working_dir` is chdir'd into for the
duration of each run, so `ansible.cfg`, relative paths and roles are resolved
relative to the project.

### Go-managed SSH transport

When `ssh.enabled` is true, Ansible uses a custom `strangle_ssh` connection
plugin that asks the Go process to perform all SSH operations. Connections are
created lazily and **kept open** in a pool, so they are reused across tasks and
across playbook runs. No `ssh` process is spawned.

```yaml
ssh:
  enabled: true
  user: kakwa              # default user when the inventory does not set one
  port: 22
  private_key_file: ~/.ssh/id_ed25519  # supports unencrypted or passphrase keys
  private_key_passphrase: ""           # optional
  use_agent: true          # keys from SSH_AUTH_SOCK
  # password: ""           # password auth (discouraged; prefer keys/agent)
  host_key_policy: api     # api | known_hosts | ca | auto | insecure
```

Authentication: `private_key_file` (with optional `-cert.pub` for OpenSSH user
certificates), `use_agent` (SSH agent), and `password` can be combined. The
first usable method wins. With `use_agent`, `SSH_AUTH_SOCK` must be set.

Host-key policies:

| Policy        | Behavior |
|---------------|----------|
| `api` (default) | Unknown keys are stored as *pending* and rejected until approved via `POST /api/hostkeys/accept`. A changed key for a known host is rejected and queued as pending. |
| `known_hosts` | Strict verification against `known_hosts_file` (default `known_hosts.strangle`, OpenSSH format). |
| `ca`          | Trust host certificates signed by a CA public key listed in `ca_files` (authorized_keys format). |
| `auto`        | Trust the first key seen per host; reject later changes. Accepted keys persist in `auto_accept_file`. |
| `insecure`    | Accept any key. Development only. |

Mark hosts with `ansible_connection=strangle_ssh` in the inventory (see
`inventory-remote.ini`). With `api` policy, the first run of a playbook will
fail with a "pending approval" error; list pending keys with `GET /api/hostkeys`,
then approve the fingerprint and re-run. Check `GET /api/ssh/stats` to confirm
`total_dials` stops growing while `active_connections` stays constant.

## Running

Requires Go (with cgo enabled), `pkg-config`, Python 3 development headers and
Ansible importable from that Python. The `make` targets configure `PKG_CONFIG`
to map cpy/v3's `python-3.12-embed` lookup onto the installed `python3-embed`
package; use them rather than plain `go build`/`go test` (or set `PKG_CONFIG`
yourself).

```sh
# build and start the server
make build
STRANGLE_INTERVAL=1m ./bin/strangle

# build and start with Go-managed SSH
STRANGLE_CONFIG=strangle-remote.yaml STRANGLE_INTERVAL=1m ./bin/strangle

# web UI (development): Vite dev server on :5173, proxies /api to :8080
make web-dev

# web UI (production): builds into web/dist, served by the Go server
make web
```

### Shutdown

`SIGINT` and `SIGTERM` trigger a graceful shutdown. The HTTP server stops
accepting connections, no further playbooks are started, and any playbook that
is already running is allowed to **finish and be persisted** before the process
exits. Send `SIGKILL` to abort a run immediately (orphaned Ansible worker
processes may be left behind in that case).

Internally, Ansible's `TaskQueueManager` replaces the process-wide SIGINT/
SIGTERM handlers with its own during a run, which would clobber the C-level
handlers Go's `os/signal` installs. The embedded runner refuses those two
`signal.signal` calls (a guard installed before Ansible is imported) so signal
delivery keeps working for the life of the process.

## Status

Playbook scheduling, PostgreSQL-backed history, per-task change traces and the
web UI are implemented and exercised by the test suite (PostgreSQL tests run
when `STRANGLE_TEST_DSN` is set).
