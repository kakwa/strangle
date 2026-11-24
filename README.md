# strangle

[![CI](https://github.com/kakwa/strangle/actions/workflows/ci.yml/badge.svg)](https://github.com/kakwa/strangle/actions/workflows/ci.yml)

Strangle: a server that applies to job postings periodically and reports on the results.
Playbooks are run **inside the Go process** through an embedded CPython interpreter (via
[`go.nhat.io/cpy/v3`](https://pkg.go.dev/go.nhat.io/cpy/v3)): no `ansible-playbook` process is spawned.

## Layout

```
cmd/strangle/        entrypoint: config, wiring, graceful shutdown
internal/
  ansible/           embedded CPython runner + Go-backed Ansible connection plugin
  config/            YAML config file + environment variables
  applier/           job + report types and the Applier interface (Ansible + stub)
  scheduler/         runs the applier over the job list on an interval
  store/             in-memory, bounded store of reports
  server/            HTTP API + serves the built web UI
  sshpool/           Go-managed persistent SSH connections with host-key policies
web/                 React (Vite) web UI, styled with Mantine
strangle.yaml        base configuration (playbook, inventory, optional auth)
strangle-remote.yaml example using the Go-managed SSH transport
```

## API

- `GET /api/health`      — liveness
- `GET /api/jobs`        — configured job postings
- `GET /api/reports`     — application reports, newest first (includes `exit_code` and `--diff` output)
- `GET /api/hostkeys`    — host keys pending approval (SSH transport only)
- `POST /api/hostkeys/accept` — body `{"host": "...", "fingerprint": "..."}`
- `POST /api/hostkeys/reject` — body `{"host": "..."}`
- `GET /api/ssh/stats`   — SSH pool counters (`active_connections`, `total_dials`)

## Configuration

Runtime settings read from the environment:

| Variable            | Default         | Description                       |
|---------------------|-----------------|-----------------------------------|
| `STRANGLE_CONFIG`   | `strangle.yaml` | Path to the YAML config file      |
| `STRANGLE_ADDR`     | `:8080`         | HTTP listen address               |
| `STRANGLE_INTERVAL` | `15m`           | How often jobs are applied to     |

The YAML file holds the Ansible parameters:

```yaml
playbook: playbook.yml   # required
inventory: inventory.ini # required

user: deploy             # optional: -u
become: true             # optional: -b
become_method: sudo      # optional: --become-method (default: sudo)
become_user: root        # optional: --become-user

# Password files instead of interactive prompts (-K/-k cannot be answered in-process):
become_password_file: /run/secrets/ansible_become
connection_password_file: /run/secrets/ansible_connection
vault_password_file: /run/secrets/ansible_vault
```

`--diff` is always added by the applier. The captured output and the playbook exit
code are stored in each report.

### Go-managed SSH transport

When `ssh.enabled` is true, Ansible uses a custom `strangle_ssh` connection plugin
that asks the Go process to perform all SSH operations. Connections are created
lazily and **kept open** in a pool, so they are reused across tasks and across
playbook runs. No `ssh` process is spawned.

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
# build and start the server (local demo)
make build
STRANGLE_INTERVAL=1m ./bin/strangle

# build and start with Go-managed SSH
STRANGLE_CONFIG=strangle-remote.yaml STRANGLE_INTERVAL=1m ./bin/strangle

# web UI (development): Vite dev server on :5173, proxies /api to :8080
make web-dev

# web UI (production): builds into web/dist, served by the Go server
make web
```

## Status

The applier is wired to the embedded Ansible runner with a Go-managed SSH
transport. The in-memory store and hardcoded job list are still placeholders.
