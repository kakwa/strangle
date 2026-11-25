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
