# Git-backed collections

A target with a `git` block in `collection.yml` is **git-backed**: strangle
owns the checkout of its upstream repository and keeps it pinned to a tracked
ref.

```yaml
collections:
  - name: site
    path: /opt/strangle/site        # the checkout directory
    git:
      url: https://git.example.com/infra/site.git
      branch: main                  # tracked ref: branch, tag or commit
      # ref: v1.2.0                 # alternative to branch (ref wins)
      # token: "..."                # optional credential for https URLs
```

- `path` doubles as the checkout directory: strangle clones the repository
  into it and applies the playbooks from there.
- The tracked ref may be a branch, a tag or a commit; `branch` is shorthand
  for `ref`.
- All git operations are in-process ([go-git](https://github.com/go-git/go-git)
  via `internal/git`); no system `git` binary is invoked.
- A `token` is embedded in https clone/fetch URLs for authenticated
  repositories.

## Startup

At startup the master syncs every git-backed target (`InitialSync`): it
clones the repository (or fetches and updates an existing checkout) to its
tracked ref, then loads the target's `strangle.yml` from the checkout. A
failure on one target does not stop the others and is reported, not fatal —
the target can be synced later through `POST /api/sync`.

## The sync webhook

`POST /api/sync` moves one target's checkout to its tracked ref — or to the
branch named in the request, which **becomes the new tracked ref** — and
reloads its `strangle.yml` from the fresh checkout:

```sh
# sync to the tracked ref
curl -X POST localhost:8080/api/sync -H "X-Strangle-Token: <token>" \
  -d '{"target": "site"}'

# switch the tracked branch and sync
curl -X POST localhost:8080/api/sync -H "X-Strangle-Token: <token>" \
  -d '{"target": "site", "branch": "release-1.4"}'
```

```json
{"target": "site", "rev": "9f2c1ab...", "changed": true}
```

When `auth.token` is set in `server.yml`, the token is required
(`Authorization: Bearer <token>` or `X-Strangle-Token: <token>`); see
[api.md](api.md).

Errors: `404` unknown target or no git-backed target configured, `400` target
without a git upstream, `502` a git failure (network, auth, or a broken
`strangle.yml` in the new checkout).

A bad push is visible but cannot freeze the target: when the reloaded
`strangle.yml` fails to load, the old playbook list is kept and the error is
reported — the checkout is not rolled back.

After a successful sync, the master refreshes the applier's view of the
target's playbooks. The scheduler reconciles its live job list every ten
seconds, so new jobs and changed `schedule` values take effect without a
restart.

## Branch templating

In a git-backed target's `strangle.yml`, the `working_dir`, `playbook`,
`inventory` and `schedule` fields are Go templates with two
variables, so the content a job runs and its cadence are selected by the
tracked ref:

| Variable | Value |
|----------|-------|
| `{{ .Branch }}` | the tracked ref (branch, tag or commit) |
| `{{ .Name }}`   | the target name |

```yaml
playbooks:
  - name: site
    playbook: playbooks/site.yml
    inventory: inventories/{{ .Branch }}.ini
    options:
      become: true
```

Every sync re-renders these paths with the current ref, so switching branches
switches the inventory (or playbook) without further configuration.

## Known limitations

- The workers build their SSH managers (for the Go-managed SSH transport)
  from the targets as they exist at worker start, and keep them for the life
  of the worker. A sync that introduces *new* playbook scopes with
  `options.ssh.enabled: true`, or that changes the SSH options (credentials,
  policy, host-key files) of an existing scope, takes effect for the master
  immediately, but the workers only see it when they are next started — i.e.,
  after a master restart (a dead worker is restarted by the supervisor with
  the fresh config).
- Playbook and inventory paths, and the run options (`user`, `become`, …) of
  existing scopes take effect immediately: the master's applier reads the
  reloaded configuration at run time, passes the paths and flags to the
  worker, and the playbooks themselves are read from disk at run time.
