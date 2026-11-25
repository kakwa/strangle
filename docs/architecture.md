# Architecture

## Process model

strangle is a single binary with two roles:

```
strangle (master)
  ├── config: server.yml, collection.yml, per-target strangle.yml
  ├── collection.Manager: git-backed targets (clone/pin/sync/reload)
  ├── worker.Manager: spawns and supervises the worker pool
  ├── scheduler + River client: enqueues playbook runs
  └── HTTP server: /api/* and the embedded web UI

strangle worker --socket /tmp/.../worker-N.sock   (× N, N = workers)
  ├── one embedded CPython interpreter + Ansible (go.nhat.io/cpy/v3)
  ├── SSH managers (one per job using the Go-managed transport)
  └── gRPC Runner service over a unix socket
```

The master never executes Ansible. It dispatches each run to a worker
process. A worker owns exactly one interpreter, so `workers: N` allows N
concurrent playbook runs; within a worker, runs are serialized by the
interpreter.

The socket directory is created in the system temp directory; each socket is
named `worker-<id>.sock`. The directory and stale sockets are removed on stop.

## The Runner service

Defined in `proto/strangle/v1/runner.proto` (generated code in the same
package):

- `Ping` — liveness probe.
- `RunPlaybook(scope, working_dir, args)` — executes one playbook in the
  worker's interpreter and returns the exit code, the captured stdout/stderr
  and an error for failures outside the interpreter (setup, I/O, a closed
  worker).

`scope` is the SSH scope key `<target>/<playbook name>` whose manager should
serve the Go-managed SSH transport for this run (empty when the playbook
does not use the transport). `args` is the full `ansible-playbook` argument
list built by the applier (`--diff`, optionally `--check` for a dry run,
`-i <inventory>`, the become flags, the playbook path).

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

## Job flow

### With a database (the production mode)

Runs flow through a postgres-backed [River](https://github.com/riverqueue/river)
queue (`internal/riverqueue`):

- The scheduler (`internal/scheduler`) owns one timer per (target, playbook)
  pair. A job's `schedule` (duration or cron) decides when it is enqueued; a
  job without one runs at startup and then every `interval` from server.yml.
  The live job list is reconciled every ten seconds, so a git sync can change
  schedules without a restart. `POST /api/run` enqueues a single job the same
  way.
- A River client in the master process consumes the `playbooks` queue with
  `MaxWorkers` = `workers`, so at most as many runs execute concurrently as
  there are worker processes.
- A run that outlives 30 minutes is canceled (its result is still recorded by
  the applier). A failed job gets up to 3 attempts in total; the next
  scheduled run re-enqueues the playbook regardless, so this only bounds
  immediate re-runs.
- The River client runs on its own context: a `SIGTERM` must not hard-cancel
  in-flight runs; shutdown drains the queue instead (see below).

### Without a database

An in-memory store is used (history is lost on restart) and runs execute
directly: the scheduler starts a background goroutine per due job, and
`POST /api/run` does the same. Both use a 30-minute timeout.

## The applier

`internal/applier` receives the run (from a River worker or directly),
dispatches it to the worker pool through the `Executor` interface, parses the
captured output with `internal/parser` (recap, per-task status, diffs,
errors), parses the target's inventory with `internal/inventory`, and records
everything through the `store.Store` interface.

After a git sync, the master calls `UpdateTargets` with the reloaded targets,
so a branch switch or a changed `strangle.yml` takes effect from the next run.

When a run fails, `internal/notify` fans the result out to the notifiers
configured on that collection in collection.yml: generic HTTP webhook, Slack
incoming webhook or SMTP email. Notifiers are independent, time out
individually and are best-effort — a broken endpoint cannot change the
recorded run result. Notification settings are kept across git syncs because
they are server-side configuration, not repository content.

## The store

`store.Store` is the persistence interface with two implementations:

- `internal/store` — in-memory; used when no DSN is configured.
- `internal/db` — GORM on PostgreSQL; tables for runs, per-host recap lines,
  append-only per-task results, per-task latest state and hosts.

Every task result parsed from a run is appended to task history, so the run
detail can show all of its statuses, diffs and errors. The separate
`task_states` table keeps only the latest state per host/task and powers the
host page; history starts at the upgrade that introduced it.

Hosts are keyed by `target + name`, so the same host name in two targets is
two distinct hosts. The web UI merges same-named hosts across targets when
rendering a single host table.

`internal/retention` runs in the master as a second ticker-driven maintenance
task. It calls `PruneRuns` once at startup and then on the configured interval,
using the server policy or a per-collection override. Only finished runs are
eligible; queued and running runs stay so a job cannot recreate a purged
record. Per-host recap and per-task history rows are deleted in the same
transaction (or under the in-memory store's lock) as their run.

`internal/hostcleanup` is a second maintenance loop with the same cadence
pattern. A host becomes eligible once its collection's inventory snapshot no
longer contains it and its last sight is older than the configured delay; its
per-host recap, latest task state and history rows are deleted with it. Host
identity includes the target, so same-named hosts in other collections are
unaffected. Like retention settings, the delay can be overridden per
collection and survives git syncs.

A worker's embedded interpreter installs the `strangle_facts` Ansible callback
alongside the Go-managed SSH connection plugin. The callback accumulates every
`ansible_facts` dictionary returned for a host (including the implicit
`Gathering Facts` task) in JSON form; the runner passes that per-host map back
over gRPC with the run output. `SaveRun` refreshes the host's `facts` jsonb
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

Host keys are persisted per scope as JSON/known_hosts files in the target
directory (shared with the master, which serves the approval API). See
[the SSH transport](ssh-transport.md) for the policies and the approval
workflow.

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
