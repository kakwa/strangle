# Configuration

Configuration is file-only: three kinds of YAML files. There are no
environment variables. The files are located with flags:

```sh
strangle-server serve [-s config/server.yml] [-c config/collection.yml]
```

| Flag | Meaning | Default |
|------|---------|---------|
| `-s` | the server.yml file | `./config/server.yml` |
| `-c` | the collection.yml file | `./config/collection.yml` |

Decoding is strict: unknown keys are rejected, and the old flat `strangle.yml`
format (a list of playbook file names plus a target-wide inventory) is not
accepted.

## server.yml

Process-level settings for the master process.

| Key | Default | Description |
|-----|---------|-------------|
| `listen_addr` | `:8080` | HTTP listen address (API and web UI). |
| `log_level` | `info` | `debug`, `info`, `warn` or `error`. JSON logs on stderr. |
| `schedule` | `@every 15m` | Fallback for jobs without their own schedule in `strangle.yml`, in robfig/cron v3 format. Jobs wait for their first scheduled time; there is no startup run. |
| `workers` | `1` | Worker child processes. Each owns one embedded Ansible interpreter, so this is the maximum number of concurrent playbook runs. |
| `database.dsn` | — | PostgreSQL DSN. Required: run history, accounts, tokens, collection memberships and the River job queue all use this database. |
| `auth.token` | — | When set, the collection `hook` endpoint (without a UI session) requires the token, either as `Authorization: Bearer <token>` or as the webhook-friendly `X-Strangle-Token: <token>` header. This static token works only on `hook` and is independent of the UI authentication below. |
| `auth.cli_token` | — | User API token (`st_…`) sent by the CLI subcommands on every API call; a client-side setting the server ignores. Tokens are scoped to one org/collection, so the configured token must match the collection the CLI reads. Can be overridden with `--token`. |
| `sandbox.enabled` | `false` | Enables Landlock isolation: each collection gets dedicated workers, its checkout is read-only and Ansible writes go to a private scratch directory. |
| `sandbox.read_only` | — | Extra absolute paths made readable to every sandboxed worker. |
| `sandbox.writable` | — | Extra absolute paths a sandboxed worker may write to. Collection directories stay read-only. |
| `retention.interval` | `1h` | How often the master prunes old runs. Only used when `max_age` or `max_count` is set. |
| `retention.max_age` | — | Delete finished runs older than this duration (Go duration syntax, e.g. `720h`). |
| `retention.max_count` | — | Keep only the newest N finished runs per collection. Can be combined with `max_age`. |
| `host_cleanup.interval` | `1h` | How often stale hosts are removed. Only used when `delay` is set. |
| `host_cleanup.delay` | — | Remove a host once it is no longer in its collection's inventory and its last sight is older than this duration. |
| `tls.cert_file` / `tls.key_file` | — | Serve the API and UI over HTTPS. Both files are required; otherwise the server uses plain HTTP. |

### Run retention

Retention is disabled until `max_age` or `max_count` is set in `server.yml`
or on a collection. Each pass deletes finished runs that are older than
`max_age` or beyond the newest `max_count`; in both cases the run's per-host
and per-task rows are deleted with it. Queued and running runs are never
purged, so an in-flight job cannot recreate a deleted record when it
finishes. A collection-level block overrides both limits, but the cadence
stays process-wide.

### Host auto clean-up

With `host_cleanup.delay` set, the master periodically removes hosts that are
no longer targeted by a collection (absent from the latest inventory snapshot)
and whose last sight is older than the delay. Their per-host recap lines,
latest task states and per-task history are removed with them. Host names are
scoped by collection, so a same-named host in another collection is kept. The
delay can be overridden per collection in collection.yml; the cadence stays
process-wide.

### HTTPS

Set both `tls.cert_file` and `tls.key_file` in server.yml to serve the API and
the embedded web UI over HTTPS on `listen_addr`. Paths are read by the master
at startup; a partial configuration is rejected. Without TLS the server keeps
listening in plain HTTP, so terminate TLS in a reverse proxy when deploying
that way.

### Collection sandboxing

With `sandbox.enabled: true` the master spawns `workers` processes **per
collection** instead of one shared pool. Runs are routed to the workers of
their collection. Landlock restricts each worker to:

- its own collection directory and working directory (read-only);
- the system paths needed by Go, Python and Ansible (read-only);
- a private scratch directory (`TMPDIR`, `HOME`, `ANSIBLE_LOCAL_TEMP`,
  `ANSIBLE_HOME` and `PYTHONPYCACHEPREFIX` point into it);
- strangle-managed SSH host-key trust files in its own collection.

A playbook therefore cannot read another collection's files or credentials,
and cannot modify its own checkout. The policy uses
`go-landlock`'s best-effort mode: older kernels enforce as much of it as they
can, and kernels without Landlock log a warning and run without isolation.
`config/server.yml` contains a commented example. The
[validating collection](../tests/sandbox/validate/README.md) can be added to a
collection.yml to verify the isolation from inside a worker.

## UI authentication

UI authentication is not a config flag: it is **on when at least one local
account exists**, and off otherwise (open server, with a startup warning).
Accounts live in the `users` table of the configured database, so a DSN in
`server.yml` is required to manage them.

```sh
# create the first account (enables authentication from the next restart on)
strangle-cli user add admin --password-stdin
strangle-cli user list
strangle-cli user remove admin
```

Passwords are hashed with argon2id and stored in the `PHC` format. With
authentication on, every `/api/v1` request needs a valid session cookie
(12 h, in memory), except `GET /api/v1/health`, `POST /api/v1/login`,
`POST /api/v1/logout`, `GET /api/v1/me`, `/api/v1/openapi.json` and `/api/docs`;
see [the API reference](api.md#authentication).

## API tokens

A local account can create **API tokens**: bearer credentials that
authenticate as that account on one org/collection pair, for CLIs, webhooks
and other integrations. A token presented on another pair is rejected. They
are managed per user in the web UI (under *Tokens*, for the selected
org/collection) or with the `token` command group:

```sh
strangle-cli token create ci --user admin --org lab --collection edge --ttl 72h  # prints the raw value once
strangle-cli token list [--user admin] [--org lab] [--collection edge]
strangle-cli token remove ci --user admin [--org lab] [--collection edge]
```

`create` requires `--user`, `--org` and `--collection`; `list` and `remove`
accept optional `--org`/`--collection` filters, and an omitted filter matches
all orgs/collections. As with users, the command group reads the DSN from `-s`
(the server.yml), so that file must set `database.dsn`.

- A token is `st_` plus 32 random bytes in hex (`st_` + 64 hex chars).
  Only its SHA-256 digest is stored; the raw value is printed once at
  creation and can never be recovered.
- Names are unique per user/scope. A token's lifetime is optional
  (`--ttl`, e.g. `72h`; absent = no expiry).
- Use a token by sending `Authorization: Bearer st_…` or
  `X-Strangle-Token: st_…`. It acts as the owning user on its own
  org/collection, including the collection's `hook` route without a session.
- The static `auth.token` (see the table above) is a separate mechanism:
  it only opens the collection `hook` route and is not a user account.
- Put a generated token in `auth.cli_token` in server.yml to use the CLI
  commands (`strangle-cli hosts`, `host`, `hostkeys`) against an
  authenticated server. The token's org/collection must match the collection
  being queried; `--token` overrides the configured value.

See [the API reference](api.md#collection-resources) for the
collection-scoped token endpoints and [the CLI section](../README.md#cli-client)
for the commands.

## collection.yml

The inventory of Ansible projects (**targets**) that strangle applies.

```yaml
collections:
  - name: home                     # identifier; unique within the collection
    path: /path/to/ansible         # the project directory (contains strangle.yml)
    git:                           # optional: upstream repository
      url: https://example.com/repo.git
      branch: main                 # tracked ref: branch, tag or commit
      # ref: v1.2.0                # alternative to branch (ref wins)
      # token: "..."               # optional credential for https URLs
```

- Relative `path` values resolve against the collection.yml directory.
- A `git` block makes the target **git-backed**: strangle clones the
  repository into `path` and keeps it pinned to the tracked ref on startup
  and on every collection `hook` call. See
  [git-backed collections](git-collections.md).

### Per-collection retention

A target can override the server's `retention.max_age` and
`retention.max_count` limits. This belongs in collection.yml so it survives a
git-backed target's `strangle.yml` reload.

```yaml
collections:
  - name: home
    path: /path/to/ansible
    retention:
      max_age: 168h      # 7 days
      max_count: 200
```

### Per-collection host clean-up

Likewise, a target can override `host_cleanup.delay` (but not the maintenance
interval) in collection.yml. This setting is server-side and survives a
git-backed target's `strangle.yml` reload.

```yaml
collections:
  - name: home
    path: /path/to/ansible
    host_cleanup:
      delay: 168h        # 7 days
```

## strangle.yml (one per target)

Lives at the root of the target directory. It names the jobs of the target;
each job is a playbook file with its own inventory and options.

```yaml
# Optional defaults inherited by every job; a job's `options` overrides them
# field by field.
# options:
#   user: kakwa
#   become: true

playbooks:
  - name: hypervisors              # job identifier (API, UI, run history)
    playbook: ./hypervisor.yml     # relative to working_dir (or absolute)
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

working_dir: playbooks             # optional; defaults to the target path
```

- `name` is the job's identifier in the API, web UI and run history;
  `playbook` is the file actually executed.
- `playbook` and `inventory` resolve against `working_dir` (absolute paths
  are used as-is); `inventory` may be a file or a directory.
- Unset values in a job's `options` fall back to the top-level `options`
  block.
- Playbook names must match `[A-Za-z0-9._-]`.

### Job schedules

Each job sets its cadence with a `schedule` in `strangle.yml`, using the
[robfig/cron v3](https://github.com/robfig/cron) standard format:

```yaml
# Optional default for jobs that do not set their own.
schedule: "*/5 * * * *"        # every five minutes

playbooks:
  - name: hypervisors
    playbook: hypervisor.yml
    inventory: inventory
    schedule: "0 3 * * 1"      # Mondays at 03:00
  - name: base_infra
    playbook: infra.yml
    inventory: inventory
    schedule: "@every 30m"     # descriptor
  - name: inherited
    playbook: inherited.yml
    inventory: inventory
    # no job or target schedule: uses server.yml's fallback
```

Precedence is **job → target → server**: a job's `schedule` wins, then the
top-level `schedule` in `strangle.yml`, then `server.yml`'s `schedule` (default
`@every 15m`). The old `interval` key is no longer accepted.

Every job waits for its first scheduled time after the master starts; there is
no startup run. A git hook that changes a target's schedules is picked up by
the scheduler within about ten seconds.

`schedule` accepts exactly what robfig/cron v3's `ParseStandard` accepts:

- a standard five-field cron expression: `minute hour day-of-month month
  day-of-week`, with `*`, `?`, lists (`1,15`), ranges (`mon-fri`) and steps
  (`*/15`). Month and weekday names may be full or three-letter (e.g. `JAN`,
  `mon`) and are case-insensitive. When both day-of-month and day-of-week are
  restricted, either one matches (Vixie cron semantics).
- the descriptors `@hourly`, `@daily` (or `@midnight`), `@weekly`, `@monthly`,
  `@yearly` (or `@annually`), and `@every <duration>` (minimum one second).

Cron times use the master's local timezone (set `TZ` for the process if it
should not be the host default). `@every` intervals are measured from the
scheduled time, not from when the previous run finishes.

### Template variables in git-backed targets

For git-backed targets, `working_dir`, `playbook`, `inventory` and `schedule`
are interpreted as Go templates with two variables, so a branch can select
different project content or cadence:

| Variable | Value |
|----------|-------|
| `{{ .Branch }}` | the tracked ref (branch, tag or commit) |
| `{{ .Name }}`   | the target name |

```yaml
playbooks:
  - name: site
    playbook: playbooks/site.yml
    inventory: inventories/{{ .Branch }}.ini   # a different inventory per branch
```

A branch switch (a collection `hook` call with a `branch` field) re-renders
these paths, so the effect is visible from the next run.

### Run options

The `options` block (target-level defaults or per-job overrides). Pointer
scalars distinguish "unset" from false/0, so for example
`become: false` explicitly disables a target-level `become: true`.

| Key | Meaning |
|-----|---------|
| `user` | SSH user (`-u`); also the SSH transport's fallback user |
| `become` | Privilege escalation (`-b`); can be explicitly set to `false` |
| `become_method` | `--become-method` (Ansible defaults to `sudo`) |
| `become_user` | `--become-user` |
| `become_password_file` | `--become-password-file` |
| `connection_password_file` | `--connection-password-file` |
| `vault_password_file` | `--vault-password-file` |
| `ssh` | Go-managed SSH transport (below); replaces/merges the top-level `ssh` block |

Password files are used instead of interactive prompts, which an embedded
interpreter cannot answer. `--diff` is always added by the applier. Dry runs
(`--check`) are supported by the runner but are not a configuration option and
scheduled runs never use them; there is no HTTP endpoint to start a run.
`working_dir` is chdir'd into for the duration of each run, so
`ansible.cfg`, relative paths and roles resolve relative to the project.

### The Go-managed SSH transport

Configured per job in `strangle.yml` under `options.ssh` (or once at the top
level under `options.ssh` as a default for all jobs). When `enabled` is true,
Ansible uses a custom `strangle_ssh` connection plugin that asks the Go
process to perform all SSH operations for that job. Each job with the
transport enabled gets its own connection pool keyed `<target>/<name>`, so
credentials, policies and pending host keys are isolated per job.

Default host-key files are named `strangle.<name>.known_hosts`,
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

Authentication: `private_key_file` (with optional `-cert.pub` for OpenSSH
user certificates), `use_agent` (SSH agent) and `password` can be combined;
the first usable method wins. With `use_agent`, `SSH_AUTH_SOCK` must be set.

To issue **short-lived user certificates** instead of using a static
`-cert.pub`, add `certificate` to the `ssh` block:

| Key | Meaning |
|-----|---------|
| `backend` | `internal` (local CA key) or `vault` (Vault SSH secrets engine) |
| `ttl` | Certificate lifetime; default `10m`, allowed `1m`–`24h` |
| `principals` | Extra valid principals; the endpoint user is always included |
| `key_id_prefix` | Prefix for certificate key IDs; default `strangle` |
| `internal.ca_key_file` | PEM private key used to sign certificates (`internal` backend) |
| `vault.addr` | Vault URL, e.g. `https://vault.example.com` (`vault` backend) |
| `vault.namespace` | Optional Vault Enterprise namespace |
| `vault.mount` / `vault.role` | SSH engine mount (default `ssh`) and signing role |
| `vault.auth_mount` | AppRole auth mount; default `approle` |
| `vault.role_id` / `vault.role_id_file` | AppRole role ID, inline or from a file |
| `vault.secret_id` / `vault.secret_id_file` | AppRole secret ID, inline or from a file |
| `vault.ca_cert_file` | Optional PEM CA bundle for Vault's TLS certificate |

A certificate backend requires `private_key_file` as the subject key and
disables `use_agent` plus any adjacent static `-cert.pub`. Connections are
re-established before their certificate expires. See
[the SSH transport](ssh-transport.md) for complete internal-CA and Vault
examples and the host-key policy reference.

Mark hosts with `ansible_connection=strangle_ssh` in the inventory (see
`tests/ssh/inventory.ini`). The full policy table and the host-key approval
workflow are in [the SSH transport](ssh-transport.md).
