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
  config/            YAML config: server.yml, collection.yml, per-target strangle.yml
  applier/           runs every target's playbooks, parses output, records results
  parser/            parses ansible-playbook output (recap, task results, diffs)
  inventory/         parses YAML inventory files into host groups and members
  scheduler/         runs a cycle on an interval
  store/             run records and the Store interface (in-memory + PostgreSQL)
  db/                GORM/PostgreSQL implementation of the Store
  server/            HTTP API + serves the built web UI
  sshpool/           Go-managed persistent SSH connections with host-key policies
web/                 React (Vite) web UI, styled with Mantine; built assets are
                     embedded into the binary (go:embed)
config/              base configuration, loaded by default
  server.yml           process-level settings (listen address, interval, DSN)
  collection.yml       the list of Ansible projects (targets) to apply
  strangle.yml         sample per-target configuration (reference)
demo/                the local demo target (applied to localhost)
  strangle.yml           per-target configuration (live)
  playbook.yml           the demo playbook
  inventory.ini          the demo inventory
tests/               test configurations
  ssh/                   Go-managed SSH transport example
    playbook.yml             the SSH demo playbook
    inventory.ini            inventory pointing at a remote host
```

## What is stored

- **Run** — one `ansible-playbook` invocation: target, playbook, start/finish,
  duration, exit code, host/changed/failed counts, full captured output.
- **HostRun** — per-run, per-host recap line (`ok`, `changed`, `failed`,
  `unreachable`, `skipped`, `rescued`, `ignored`).
- **TaskState** — per host × task (play + task name): the last status, when
  it last ran, the last diff and the last error. Updated on every run.
- **Host** — every host (identified by target + name, so the same name in
  two targets is two hosts) seen in an inventory or recap; `active` tracks
  whether it was in scope of a run but missing from the recap
  ("disappeared").

## API

- `GET /api/health`      — liveness
- `GET /api/stats`       — fleet statistics (hosts, active, disappeared, run counts, durations)
- `GET /api/runs`        — runs, newest first (`?limit=`)
- `GET /api/hosts`       — all hosts with their last status and disappearance state
- `GET /api/hosts/{host}/runs`    — the last runs that included the host (default 10)
- `GET /api/hosts/{host}/changes` — the host's task traces, newest first (default 200)
- `GET /api/playbooks`   — playbook job names, keyed by target name
- `POST /api/run`        — body `{"target": "demo", "playbook": "site"}`;
  `playbook` is the job name from `strangle.yml`, not a file path. Starts the
  run in the background and returns `202` (unknown target or job names are
  rejected)
- `GET /api/hostkeys`    — host keys pending approval (SSH transport only)
- `POST /api/hostkeys/accept` — body `{"target": "demo/site", "host": "...", "fingerprint": "..."}`
- `POST /api/hostkeys/reject` — body `{"target": "demo/site", "host": "..."}`

For the host-key endpoints, `target` is the SSH scope key
`<target>/<playbook name>`; it appears as `target` in `/api/hostkeys` output.
- `GET /api/ssh/stats`   — SSH pool counters (`active_connections`, `total_dials`)

## Web UI

Single page (React + Mantine) with three views, polled every 10 seconds:

- **Dashboard** — fleet stats and the most recent runs.
- **Hosts** — the host table with last status and disappearance state.
- **Host detail** — the last 10 runs for that host, plus its task traces with
  expandable diffs and errors.

## Configuration

Configuration is file-only: three kinds of YAML files, located with the `-s`
and `-c` flags (defaults `./config/server.yml` and `./config/collection.yml`).
There are no environment variables.

```sh
strangle [-s config/server.yml] [-c config/collection.yml]
```

### server.yml — process-level settings

| Key             | Default         | Description                          |
|-----------------|-----------------|--------------------------------------|
| `listen_addr`   | `:8080`         | HTTP listen address                  |
| `log_level`     | `info`          | `debug`, `info`, `warn` or `error`   |
| `interval`      | `15m`           | How often a full cycle over every target is run |
| `database.dsn`  | —               | PostgreSQL DSN; without a DSN an in-memory store is used |
| `auth.token`    | —               | Placeholder, parsed but not enforced yet |

### collection.yml — the targets

The inventory of Ansible projects (targets) that strangle applies. Every
target is a directory containing a `strangle.yml`. Relative `path` values
resolve against this file's directory:

```yaml
targets:
  - name: home
    path: /path/to/ansible        # the project directory (contains strangle.yml)
    # git:                        # upstream repository: placeholder, not used yet
    #   url: https://example.com/repo.git
    #   branch: main
```

### strangle.yml — one per target

Lives at the root of the target directory. Each entry of `playbooks` is a named
job with its own playbook file, inventory and options:

```yaml
# Optional defaults inherited by every job; a job's `options` overrides them
# field by field.
# options:
#   user: kakwa
#   become: true

playbooks:
  - name: hypervisors
    playbook: ./hypervisor.yml
    inventory: ./inventory/hypervisor.yml
    options:
      user: kakwa
      become: true
  - name: base_infra
    playbook: ./infra.yml
    inventory: ./inventory/infra.yml
    options:
      user: kakwa
      become: true

working_dir: playbooks    # optional; defaults to the target path
```

`name` is the job's identifier in the API, web UI and run history; `playbook`
is the file actually executed. Both `playbook` and `inventory` are resolved
against `working_dir` (absolute paths are used as-is). Unset values in a job's
`options` fall back to the top-level `options` block. The full option set is:

| Key | Meaning |
|-----|---------|
| `user` | SSH user (`-u`); also the SSH transport's fallback user |
| `become` | Privilege escalation (`-b`); can be explicitly set to `false` |
| `become_method` | `--become-method` (Ansible defaults to `sudo`) |
| `become_user` | `--become-user` |
| `become_password_file` | `--become-password-file` |
| `connection_password_file` | `--connection-password-file` |
| `vault_password_file` | `--vault-password-file` |
| `ssh` | Go-managed SSH transport (see below); replaces/merges the top-level `ssh` block |

Password files are used instead of interactive prompts, which an embedded
interpreter cannot answer. `--diff` is always added by the applier.
`working_dir` is chdir'd into for the duration of each run, so `ansible.cfg`,
relative paths and roles are resolved relative to the project. Unknown keys
are rejected, so the old flat `playbooks`/`inventory` format is not accepted.

The repository is a working example: `config/server.yml` and
`config/collection.yml` configure a local `demo` target (the `demo/`
directory, applied to localhost), and `config/strangle.yml` is a reference for
the per-target file each target directory carries.

### Go-managed SSH transport

Configured per job in `strangle.yml` under `options.ssh` (or once at the top
level under `options.ssh` as a default for all jobs). When `enabled` is true,
Ansible uses a custom `strangle_ssh` connection plugin that asks the Go
process to perform all SSH operations for that job. Each job with the
transport enabled gets its own connection pool keyed `<target>/<name>`, so
credentials, policies and pending host keys are isolated per job. Default
host-key files are named `strangle.<name>.known_hosts`,
`strangle.<name>.auto.json` and `strangle.<name>.pending.json` in the target
directory; set the file keys explicitly to share or relocate them.
Connections are created lazily and **kept open** in a pool, so they are reused
across tasks and across playbook runs. No `ssh` process is spawned.

```yaml
options:
  ssh:
    enabled: true
    user: kakwa            # default user when the inventory does not set one
                           # (defaults to the job's user)
    port: 22
    private_key_file: ~/.ssh/id_ed25519  # supports unencrypted or passphrase keys
    private_key_passphrase: ""           # optional
    use_agent: true        # keys from SSH_AUTH_SOCK
    # password: ""         # password auth (discouraged; prefer keys/agent)
    host_key_policy: api   # api | known_hosts | ca | auto | insecure
```

Authentication: `private_key_file` (with optional `-cert.pub` for OpenSSH user
certificates), `use_agent` (SSH agent), and `password` can be combined. The
first usable method wins. With `use_agent`, `SSH_AUTH_SOCK` must be set.

Host-key policies:

| Policy        | Behavior |
|---------------|----------|
| `api` (default) | Unknown keys are stored as *pending* and rejected until approved via `POST /api/hostkeys/accept`. A changed key for a known host is rejected and queued as pending. |
| `known_hosts` | Strict verification against `known_hosts_file` (default `<target dir>/strangle.<name>.known_hosts`, OpenSSH format). |
| `ca`          | Trust host certificates signed by a CA public key listed in `ca_files` (authorized_keys format). |
| `auto`        | Trust the first key seen per host; reject later changes. Accepted keys persist in `auto_accept_file`. |
| `insecure`    | Accept any key. Development only. |

Mark hosts with `ansible_connection=strangle_ssh` in the inventory (see
`tests/ssh/inventory.ini`). With `api` policy, the first run of a playbook will
fail with a "pending approval" error; list pending keys with `GET /api/hostkeys`,
then approve the fingerprint and re-run. Check `GET /api/ssh/stats` to confirm
`total_dials` stops growing while `active_connections` stays constant.

## Running

Requires Go (with cgo enabled), Node.js, `pkg-config`, Python 3 development
headers and Ansible importable from that Python. The `make` targets configure
`PKG_CONFIG` to map cpy/v3's `python-3.12-embed` lookup onto the installed
`python3-embed` package; use them rather than plain `go build`/`go test` (or
set `PKG_CONFIG` yourself).

The web UI is built with Vite and **embedded into the binary** (`go:embed`),
so `bin/strangle` is self-contained — no `web/dist` needed at runtime.
`make build` rebuilds the UI before compiling; a plain `go build` works on a
fresh checkout via a committed placeholder page, but bakes that placeholder in.

```sh
# build the UI (into web/dist) and the server, UI embedded
make build
./bin/strangle                      # reads ./config/server.yml and ./config/collection.yml

# or point at explicit config files
./bin/strangle -s config/server.yml -c config/collection.yml

# web UI (development): Vite dev server on :5173, proxies /api to :8080
make web-dev

# web UI (production): build into web/dist only
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
