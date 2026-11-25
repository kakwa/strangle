# The Go-managed SSH transport

By default Ansible runs the `ssh` binary for its `ssh` connection. strangle
offers an alternative: a **Go-managed SSH transport** in which the Go process
performs all SSH operations for a job. No `ssh` process is spawned;
connections are created lazily and **kept open** in a pool, so they are
reused across tasks and across playbook runs.

## How it works

1. Enable the transport per job in `strangle.yml` (`options.ssh.enabled:
   true`, or once at the top level as the default for all jobs of the
   target).
2. Mark the hosts that should use it in the inventory with
   `ansible_connection=strangle_ssh` (see `tests/ssh/inventory.ini`).
3. Inside the worker process, Ansible's custom `strangle_ssh` connection
   plugin forwards every operation (command execution, SFTP upload/download)
   to Go, which serves it from the job's connection pool.

Each job with the transport enabled gets its own connection pool and
host-key store, keyed by the **scope** `<target>/<playbook name>`, so
credentials, policies and pending host keys are isolated per job. The
manager is built in the worker process that runs the playbooks; the master
keeps its own view for the approval API (below), and the two share trust
decisions through the per-scope files on disk.

## Authentication

Configured in the `ssh` block; the first usable method wins:

| Method | Keys | Notes |
|--------|------|-------|
| Private key | `private_key_file`, `private_key_passphrase` | Unencrypted or passphrase-protected keys. An adjacent `<file>-cert.pub` is picked up automatically for OpenSSH user certificates. |
| SSH agent | `use_agent: true` | Keys from `SSH_AUTH_SOCK`, which must be set (in the worker environment). |
| Password | `password` | Discouraged; prefer keys or the agent. |

`user` (defaulting to the job's `user` option) and `port` (default 22) are
taken per host from the inventory, with these as fallbacks.

### Ephemeral user certificates

Set `options.ssh.certificate` to replace long-lived `-cert.pub` files with
short-lived SSH **user** certificates. This is orthogonal to `host_key_policy`,
which verifies the *server's* key or certificate.

```yaml
options:
  ssh:
    enabled: true
    private_key_file: /run/secrets/ssh/id_ed25519
    host_key_policy: api
    certificate:
      backend: internal       # internal | vault
      ttl: 10m                # default 10m; 1m–24h
      principals: [deploy]    # the endpoint user is always added
      key_id_prefix: strangle
      internal:
        ca_key_file: /run/secrets/ssh_ca_key
```

When a certificate backend is configured, `private_key_file` is required and
is the subject key: only its public half is signed. `use_agent` and an adjacent
`<private_key_file>-cert.pub` are ignored, so a stale certificate cannot be
used as a long-lived fallback. The same option block can be set at the
target's top level and overridden per playbook.

A fresh certificate is issued lazily on the first dial of each scope and
reissued shortly before expiry. Pooled connections are closed at that refresh
deadline, so a subsequent task reconnects with the new certificate; a long
running task is never interrupted mid-execution. The master builds the same
provider for its approval API but does not dial, so no certificate is issued
there.

**Internal CA** (`backend: internal`) signs with an unencrypted PEM private key
given by `internal.ca_key_file`. Keep this file outside the collection and
readable only by the worker (or use the Vault backend), because playbook code
runs in the same process. The subject key, CA key and Vault credential/CA files
are automatically added to the sandbox's read-only paths; they are never made
writable.

**Vault** (`backend: vault`) logs in through an AppRole and calls the SSH
secrets engine's `sign` endpoint. Credentials should be mounted as files:

```yaml
options:
  ssh:
    enabled: true
    private_key_file: /run/secrets/ssh/id_ed25519
    certificate:
      backend: vault
      ttl: 15m
      vault:
        addr: https://vault.example.com
        namespace: team-a         # optional Vault Enterprise namespace
        mount: ssh                # SSH secrets engine mount (default ssh)
        role: strangle
        auth_mount: approle       # default approle
        role_id_file: /run/secrets/vault_role_id
        secret_id_file: /run/secrets/vault_secret_id
        ca_cert_file: /etc/ssl/vault-ca.pem  # optional TLS trust
```

The inline `role_id`/`secret_id` keys are also accepted but file-based values
are strongly preferred. The AppRole token is cached until shortly before its
lease expires. Only `public_key`, `valid_principals`, `ttl` and `key_id` are
sent to Vault; the subject private key never leaves the process. SSH options
are frozen when a worker starts (see
[git-backed collections](git-collections.md)), so a rotated AppRole secret or
CA key takes effect on the next worker build.

## Host-key policies

`host_key_policy` selects how unknown or changed host keys are handled:

| Policy | Behavior |
|--------|----------|
| `api` (default) | Unknown keys are stored as *pending* and the connection is refused until approved via the API. A changed key for a known host is refused and queued with status `rejected`. Both are resolved through the approval API. |
| `known_hosts` | Strict verification against `known_hosts_file` (OpenSSH format). |
| `ca` | Trust host certificates signed by a CA public key listed in `ca_files` (authorized_keys format). |
| `auto` | Trust the first key seen per host; reject later changes. Accepted keys persist in `auto_accept_file`. |
| `insecure` | Accept any key. Development only. |

Fingerprints use the `SHA256:` format (OpenSSH `ssh-keygen -lf` style).
Approval is per collection: the endpoints below live under
`/api/v1/orgs/{org}/collections/{collection}` and are checked against the
caller's collection membership. The SSH scope in the JSON body
(`<target>/<playbook name>`) must belong to the collection in the URL.

### The approval workflow (`api` policy)

1. The first run of a playbook reaches a host with an unknown key: the task
   fails with a "pending approval" error and the key is stored as pending.
2. List the pending keys:

   ```sh
   curl localhost:8080/api/v1/orgs/default/collections/site/hostkeys
   ```

   ```json
   [
     {"target": "site/prod", "host": "10.0.0.9", "fingerprint": "SHA256:...",
      "public_key": "ssh-ed25519 AAAA...", "status": "pending", "first_seen": "..."}
   ]
   ```

3. Approve the fingerprint (or reject it):

   ```sh
   curl -X POST localhost:8080/api/v1/orgs/default/collections/site/hostkeys/accept \
     -d '{"target": "site/prod", "host": "10.0.0.9", "fingerprint": "SHA256:..."}'
   ```

4. Re-run the playbook; the connection now succeeds and the key is trusted
   for future runs.

A key change for an approved host is refused the same way: the old key stays
approved, and the new key is queued with status `rejected`. It can be
approved the same way as an unknown key — approving it replaces the trusted
key.

## Host-key files

Per scope, in the target directory by default:

| Key | Default | Purpose |
|-----|---------|---------|
| `known_hosts_file` | `strangle.<name>.known_hosts` | Known hosts in OpenSSH format, verified by the `known_hosts` policy (maintained by you) |
| `pending_file` | `strangle.<name>.pending.json` | Keys pending approval (the `api` and `auto` policies) |
| `auto_accept_file` | `strangle.<name>.auto.json` | Approved keys (the `api` and `auto` policies) |
| `ca_files` | — | CA public keys, one per file in authorized_keys format (`ca` policy) |

`<name>` is the job name. Set the keys explicitly to share or relocate the
files between jobs.

## Observability

`GET /api/v1/orgs/{org}/collections/{collection}/ssh/stats` reports
`active_connections` and `total_dials` per scope of that collection (of the
master's managers; the playbook connections themselves are pooled inside the
workers). When connections are being reused, a host that is reached on every
run contributes dials only the first time.
