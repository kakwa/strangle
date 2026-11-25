# Architecture

## Process model

strangle has two binaries: `strangle-server` runs the master and worker
processes, while `strangle-cli` holds the operator-facing commands (the API
client and the user/token/collection administration commands).

```
strangle-server serve (master)
  ├── config: server.yml, collection.yml, per-target strangle.yml
  ├── collection.Manager: git-backed targets (clone/pin/sync/reload)
  ├── worker.Manager: spawns and supervises the worker pool
  ├── scheduler + River client: enqueues playbook runs
  └── HTTP server: /api/*, /metrics and the embedded web UI

strangle-server worker --socket /tmp/.../worker-N.sock   (× N, N = workers)
  ├── one embedded CPython interpreter + Ansible (go.nhat.io/cpy/v3)
  ├── SSH managers (one per job using the Go-managed transport)
  └── JSON-RPC worker API over a unix socket
```

The master never executes Ansible. It dispatches each run to a worker
process. A worker owns exactly one interpreter, so `workers: N` allows N
concurrent playbook runs; within a worker, runs are serialized by the
interpreter.

The socket directory is created in the system temp directory; each socket is
named `worker-<id>.sock`. The directory and stale sockets are removed on stop.

## The worker protocol

`internal/worker` defines a two-method JSON-RPC service on the worker's unix
socket (`rpc_*.go`). It uses only the standard library (`net/rpc` with the
`net/rpc/jsonrpc` codec): there is no schema and no generated code.

- `Worker.Ping` — liveness probe.
- `Worker.RunPlaybook` — executes one playbook in the worker's interpreter and
  returns the exit code, the captured stdout/stderr, per-host facts and an
  error for failures outside the interpreter (setup, I/O, a closed worker).

The request carries `collection`, `scope`, `working_dir` and `args`.
`scope` is the SSH scope key `<target>/<playbook name>` whose manager should
serve the Go-managed SSH transport for this run (empty when the playbook
does not use the transport). `args` is the full `ansible-playbook` argument
list built by the applier (`--diff`, optionally `--check` for a dry run,
`-i <inventory>`, the become flags, the playbook path). Cancelling the
master's call stops the master from waiting; the worker keeps the interpreter
busy until the run finishes, so the next run cannot overlap it.

## Worker supervision

The `worker.Manager` keeps the pool alive:

- A health `Ping` goes to every worker every 10 seconds (10 s timeout).
- Two consecutive failed pings kill a live process; an unexpected exit is
  detected immediately.
- Each dead slot is restarted with exponential backoff (500 ms doubling to
  30 s; the first attempt is immediate).
- A slot that fails 10 consecutive times is given up on: the manager's
  `Down` channel closes and the master logs an error — playbook runs will
  fail until the master is restarted. Failures are forgiven if a worker
  stays up for 5 minutes.

`GET /api/workers` reports each slot: `id`, `pid`, `socket`, `alive`,
`jobs` (in-flight) and `restarts`.

Dispatch is round-robin over the live slots (dead or respawning slots are
skipped). A dispatch to a busy slot is fine: runs on one worker are
serialized inside that worker, so extra jobs queue behind the in-flight one.

`GET /metrics` is rendered on demand by the HTTP server from the same store
and worker status used by the API, plus a few Go runtime gauges. It needs no
configuration and is intentionally open like `/api/health`; see the
[API reference](api.md#get-metrics) for the exported series.

## Job flow

Runs flow through a postgres-backed [River](https://github.com/riverqueue/river)
queue (`internal/riverqueue`):

- The scheduler (`internal/scheduler`) owns one timer per (target, playbook)
  pair. A job's `schedule` is a robfig/cron v3 expression (or descriptor), or
  it inherits the server fallback; it waits for its first tick after the
  master starts. The live job list is reconciled every ten seconds, so a git
  sync can change schedules without a restart. `POST /api/run` enqueues a
  single job the same way.
- A River client in the master process consumes the `playbooks` queue with
  `MaxWorkers` = `workers`, so at most as many runs execute concurrently as
  there are worker processes.
- A run that outlives 30 minutes is canceled (its result is still recorded by
  the applier). A failed job gets up to 3 attempts in total; the next
  scheduled run re-enqueues the playbook regardless, so this only bounds
  immediate re-runs.
- The River client runs on its own context: a `SIGTERM` must not hard-cancel
  in-flight runs; shutdown drains the queue instead (see below).
- River tables hold only queue metadata (job state, attempts, timestamps).
  The applier writes every result through `db.Store` into the run-history
  tables: `runs`, `host_runs`, `task_states` and `task_run_results`.

## The applier

`internal/applier` receives the run from a River worker, dispatches it to the
worker pool through the `Executor` interface, parses the captured output
(recap, per-task status, diffs, errors), parses the target's inventory in
`inventory.go`, and records everything through the `db.Store` interface.

After a git sync, the master calls `UpdateTargets` with the reloaded targets,
so a branch switch or a changed `strangle.yml` takes effect from the next run.

## The store

`db.Store` is the persistence interface. Its only implementation is
`internal/db` — GORM on PostgreSQL, with tables for runs, per-host recap
lines, append-only per-task results, per-task latest state and hosts. There is
no in-memory fallback; `internal/db` also defines the interface and shared
value types.

Every task result parsed from a run is appended to task history, so the run
detail can show all of its statuses, diffs and errors. The separate
`task_states` table keeps only the latest state per host/task and powers the
host page; history starts at the upgrade that introduced it.

Hosts are keyed by `target + name`, so the same host name in two targets is
two distinct hosts. The web UI merges same-named hosts across targets when
rendering a single host table.

`internal/maintenance` runs retention in the master as a ticker-driven task.
It calls `PruneRuns` once at startup and then on the configured interval,
using the server policy or a per-collection override. Only finished runs are
eligible; queued and running runs stay so a job cannot recreate a purged
record. Per-host recap and per-task history rows are deleted in the same
transaction as their run.

Host cleanup is the second maintenance task, with the same cadence pattern.
A host becomes eligible once its collection's inventory snapshot no
longer contains it and its last sight is older than the configured delay; its
per-host recap, latest task state and history rows are deleted with it. Host
identity includes the target, so same-named hosts in other collections are
unaffected. Like retention settings, the delay can be overridden per
collection and survives git syncs.

A worker's embedded interpreter installs the `strangle_facts` Ansible callback
alongside the Go-managed SSH connection plugin. The callback accumulates every
`ansible_facts` dictionary returned for a host (including the implicit
`Gathering Facts` task) in JSON form; the runner passes that per-host map back
with the run output. `SaveRun` refreshes the host's `facts` jsonb
column only when the new run returned facts, so runs with `gather_facts: false`
never erase the last known snapshot. Facts are exposed on the authenticated
host endpoints and rendered in the host detail view.

## Git-backed collections

`internal/collection` keeps the checkouts of git-backed targets, pinned to
their tracked ref. All git operations are in-process (go-git, see
`internal/git`); no system `git` binary is invoked. Startup clones (or fetches
and updates) the checkouts; `POST /api/sync` moves a checkout to its tracked
ref — or to a supplied branch, which becomes the new tracked ref — and
reloads the target's `strangle.yml`. See
[git-backed collections](git-collections.md).

## The SSH transport

When a job uses the Go-managed SSH transport, Ansible talks to a custom
`strangle_ssh` connection plugin registered inside the worker's interpreter.
The plugin forwards every SSH operation to Go in the same worker process,
which maintains a persistent connection pool per job scope
(`<target>/<playbook name>`). Connections are created lazily and kept open,
so they are reused across tasks and across runs. No `ssh` process is spawned.

If `options.ssh.certificate` is configured, the pool asks a signer provider
for a short-lived user certificate whenever it opens a connection: an
internal CA signs locally, or Vault's SSH engine signs through AppRole. The
provider caches the certificate and the pool drops connections at its refresh
deadline, so later tasks reconnect with a fresh certificate. This is separate
from host-key verification.

Host keys are persisted per scope as JSON/known_hosts files in the target
directory (shared with the master, which serves the approval API). See
[the SSH transport](ssh-transport.md) for the policies, certificate setup and
the approval workflow.

## Shutdown order

On `SIGINT`/`SIGTERM` the master shuts down in an order that protects the
history of in-flight runs (they execute inside workers and write to the
store as they finish):

1. Stop the scheduler (no new jobs are enqueued).
2. Stop the retention and host clean-up loops (no purge starts during shutdown).
3. Drain the River queue (up to 10 minutes), letting in-flight runs finish.
4. Stop the workers (terminate the child processes).
5. Close the store.

The HTTP server shuts down in parallel (5-second grace). Ansible's
`TaskQueueManager` normally replaces the process SIGINT/SIGTERM handlers
during a run; the embedded runner refuses those two `signal.signal` calls (a
guard installed before Ansible is imported) so Go's handlers stay in effect.
