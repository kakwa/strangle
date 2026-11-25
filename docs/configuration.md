# Configuration

Configuration is file-only: three kinds of YAML files. There are no
environment variables. The files are located with flags:

```sh
strangle serve [-s config/server.yml] [-c config/collection.yml]
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
| `interval` | `15m` | Fallback for jobs without their own schedule in `strangle.yml`. Such jobs run immediately at startup, then on every tick (the original behaviour). |
| `workers` | `1` | Worker child processes. Each owns one embedded Ansible interpreter, so this is the maximum number of concurrent playbook runs. |
| `database.dsn` | — | PostgreSQL DSN. Without a DSN an in-memory store is used (history is lost on restart) and runs execute directly instead of through the job queue. |
| `auth.token` | — | When set, `POST /api/sync` (without a UI session) requires the token, either as `Authorization: Bearer <token>` or as the webhook-friendly `X-Strangle-Token: <token>` header. Independent of the UI authentication below. |
| `sandbox.enabled` | `false` | Enables Landlock isolation: each collection gets dedicated workers, its checkout is read-only and Ansible writes go to a private scratch directory. |
| `sandbox.read_only` | — | Extra absolute paths made readable to every sandboxed worker. |
| `sandbox.writable` | — | Extra absolute paths a sandboxed worker may write to. Collection directories stay read-only. |
| `retention.interval` | `1h` | How often the master prunes old runs. Only used when `max_age` or `max_count` is set. |
| `retention.max_age` | — | Delete finished runs older than this duration (Go duration syntax, e.g. `720h`). |
| `retention.max_count` | — | Keep only the newest N finished runs per collection. Can be combined with `max_age`. |

### Run retention

Retention is disabled until `max_age` or `max_count` is set in `server.yml`
or on a collection. Each pass deletes finished runs that are older than
`max_age` or beyond the newest `max_count`; in both cases the run's per-host
and per-task rows are deleted with it. Queued and running runs are never
purged, so an in-flight job cannot recreate a deleted record when it
finishes. A collection-level block overrides both limits, but the cadence
stays process-wide.

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
[validating collection](../sandbox/validate/README.md) can be added to a
collection.yml to verify the isolation from inside a worker.

## UI authentication

UI authentication is not a config flag: it is **on when at least one local
account exists**, and off otherwise (open server, with a startup warning).
Accounts live in the `users` table of the configured database, so a DSN in
`server.yml` is required to manage them.

```sh
# create the first account (enables authentication from the next restart on)
strangle user add admin --password-stdin
strangle user list
strangle user remove admin
```

Passwords are hashed with argon2id and stored in the `PHC` format. With
authentication on, every `/api/*` request needs a valid session cookie
(12 h, in memory), except `GET /api/health`, `POST /api/login`,
`POST /api/logout` and `GET /api/me`; see [the API reference](api.md#authentication).

## API tokens

A local account can create **API tokens**: bearer credentials that
authenticate any `/api/*` request as that account, for CLIs, webhooks and
other integrations. They are managed per user in the web UI (under
*Tokens*) or with the `token` command group:

```sh
strangle token create ci --user admin --ttl 72h   # prints the raw value once
strangle token list [--user admin]                # all users' tokens without --user
strangle token remove ci --user admin
```

As with users, the command group needs a database, so `-s` (the
server.yml) must point at a config with `database.dsn` set.

- A token is `st_` plus 32 random bytes in hex (`st_` + 64 hex chars).
  Only its SHA-256 digest is stored; the raw value is printed once at
  creation and can never be recovered.
- Names are unique per user. A token's lifetime is optional
  (`--ttl`, e.g. `72h`; absent = no expiry).
- Use a token by sending `Authorization: Bearer st_…` or
  `X-Strangle-Token: st_…`. It acts as the owning user: with
  authentication on it satisfies the session requirement on every
  `/api/*` endpoint, and it also opens `POST /api/sync` without a session.
- The static `auth.token` (see the table above) is a separate mechanism:
  it only opens `POST /api/sync` and is not a user account.

See [the API reference](api.md#api-tokens-self-service) for the
self-service endpoints.

## collection.yml

The inventory of Ansible projects (**targets**) that strangle applies.

```yaml
targets:
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
  and on every `POST /api/sync`. See
  [git-backed collections](git-collections.md).

### Failure notifications

A target can declare any number of notifiers. They fire when a run finishes
with a non-zero Ansible exit code or a runner error; successful runs are
silent. Notifications belong in collection.yml, never in a git-backed
`strangle.yml`, so repository content cannot redirect credentials.

```yaml
targets:
  - name: home
    path: /path/to/ansible
    notifications:
      - type: webhook                       # any HTTP endpoint
        url: https://example.com/strangle-hook
        headers:                            # optional extra request headers
          Authorization: Bearer secret
        timeout: 5s                         # default: 10s
      - type: slack                         # Slack incoming webhook
        url: https://hooks.slack.com/services/T/B/X
      - type: email
        from: strangle@example.com
        to: [ops@example.com, oncall@example.com]
        smtp: smtp.example.com:587          # host:port
        username: strangle                  # optional
        password_file: /run/secrets/smtp    # or `password`, not both
```

| Field | Applies to | Meaning |
|-------|-----------|---------|
| `type` | all | `webhook`, `slack` or `email` |
| `url` | webhook, slack | absolute `http`/`https` URL; notified with `POST` |
| `headers` | webhook, slack | extra HTTP headers (e.g. a bearer token) |
| `from`, `to`, `smtp` | email | sender, recipients and `host:port` of the SMTP server |
| `username`, `password` / `password_file` | email | optional SMTP PLAIN auth; a relative `password_file` resolves against collection.yml |
| `timeout` | all | per-notifier timeout, default `10s` |

Webhooks receive a JSON object with `event` (`run_failed`), `org`,
`collection`, `playbook`, timestamps, `duration_ms`, `exit_code`, host counts
and a bounded tail of the playbook output (4 KB). Slack and email receive the
same information in a human-readable message. Notifiers run in parallel and
their failures are logged but never change the recorded run result.

### Per-collection retention

A target can override the server's `retention.max_age` and
`retention.max_count` limits. Like notifications, this belongs in
collection.yml so it survives a git-backed target's `strangle.yml` reload.

```yaml
targets:
  - name: home
    path: /path/to/ansible
    retention:
      max_age: 168h      # 7 days
      max_count: 200
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

Each job can set its own cadence in `strangle.yml`, either as a duration
(`interval`) or as a cron expression (`schedule`):

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
    interval: 30m              # duration shorthand
  - name: manual_only
    playbook: manual.yml
    inventory: inventory
    schedule: "@off"           # never scheduled automatically
```

Precedence is **job → target → server**: a job's `schedule`/`interval` wins,
then the top-level `schedule`/`interval` in `strangle.yml`, then
`server.yml`'s `interval`. Setting both `schedule` and `interval` at the same
level is a configuration error.

A job with an explicit schedule waits for its first matching time after the
master starts. A job with no explicit schedule keeps the legacy behaviour: it
runs once at startup and then every `server.yml` interval. A git sync that
changes a target's schedules is picked up by the scheduler within about ten
seconds.

`schedule` accepts:

- a Go duration such as `30m`, `1h30m` or `30s` (also `@every 30s`);
- a standard five-field cron expression: `minute hour day-of-month month
  day-of-week`, with `*`, `?`, lists (`1,15`), ranges (`mon-fri`) and steps
  (`*/15`). Month and weekday names may be full or three-letter and are
  case-insensitive; `0` and `7` both mean Sunday. When both day-of-month and
  day-of-week are restricted, either one matches (Vixie cron semantics).
- the aliases `@hourly`, `@daily` (or `@midnight`), `@weekly`, `@monthly`,
  `@yearly` (or `@annually`);
- `@off` (or quoted `"off"`) to disable automatic runs for that job.

Cron times use the master's local timezone (set `TZ` for the process if it
should not be the host default). Durations are measured from when the previous
run finishes.

### Template variables in git-backed targets

For git-backed targets, `working_dir`, `playbook`, `inventory`, `schedule` and
`interval` are interpreted as Go templates with two variables, so a branch can
select different project content or cadence:

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

A branch switch (`POST /api/sync` with a `branch` field) re-renders these
paths, so the effect is visible from the next run.

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
interpreter cannot answer. `--diff` is always added by the applier. A
collection-scoped run request may set `"check": true` to add `--check` for
that single run; this is not a configuration option and scheduled runs never
use it. `working_dir` is chdir'd into for the duration of each run, so
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

Mark hosts with `ansible_connection=strangle_ssh` in the inventory (see
`tests/ssh/inventory.ini`). The full policy table and the host-key approval
workflow are in [the SSH transport](ssh-transport.md).
