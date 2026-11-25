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
| `interval` | `15m` | How often a full cycle over every target's playbooks is run. A cycle runs immediately at startup, then on every tick. |
| `workers` | `1` | Worker child processes. Each owns one embedded Ansible interpreter, so this is the maximum number of concurrent playbook runs. |
| `database.dsn` | — | PostgreSQL DSN. Without a DSN an in-memory store is used (history is lost on restart) and runs execute directly instead of through the job queue. |
| `auth.token` | — | When set, `POST /api/sync` (without a UI session) requires the token, either as `Authorization: Bearer <token>` or as the webhook-friendly `X-Strangle-Token: <token>` header. Independent of the UI authentication below. |

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

### Template variables in git-backed targets

For git-backed targets, `working_dir`, `playbook` and `inventory` are
interpreted as Go templates with two variables, so a branch can select
different project content:

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
interpreter cannot answer. `--diff` is always added by the applier.
`working_dir` is chdir'd into for the duration of each run, so `ansible.cfg`,
relative paths and roles resolve relative to the project.

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
