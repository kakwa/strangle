# Seed dataset for manual testing

`tests/seed` is a deterministic generator that fills a **fresh** strangle
database with realistic test data through the real `store` APIs (not raw SQL):
organizations, collections, playbooks, hosts, facts, task state, and past
playbook runs — some successful, some failed.

It can also write the matching `server.yml`, `collection.yml` and per-target
`strangle.yml` files, so the server sees exactly the same orgs and collections
that were seeded into the database, plus three local UI accounts with different
collection access.

## Why matching config matters

`collection.yml` is the source of truth for collections. On startup the server
runs `SyncCollections`, which **deletes any database collection (and its hosts
and run history) that is not in the config**. Seeding a database and then
starting the server with the default `config/collection.yml` would therefore
erase most of the dataset. Always pass the generated config with `-s` / `-c` as
shown below.

## 1. Initialize the database

Strangle creates its own tables: `store.Open` applies the GORM migrations and
the River queue applies its own on first start. "Init" just means creating an
**empty** database. The full server requires PostgreSQL (the job queue is
PostgreSQL-only); SQLite is supported by the store and is useful for inspecting
the dataset directly, but cannot run the server.

```sh
createdb strangle_seed
# or: psql -d postgres -c 'CREATE DATABASE strangle_seed'
```

If the database already contains a seeded dataset, drop and recreate it:

```sh
dropdb strangle_seed && createdb strangle_seed
```

No migration command is needed.

## 2. Seed it and generate the config

From the repository root:

```sh
go run ./tests/seed \
  -dsn 'postgres://localhost/strangle_seed?sslmode=disable' \
  -config-dir /tmp/opencode/strangle-seed-config
```

Adjust the DSN for your user/password/host. `-config-dir` may point anywhere;
the generator writes `server.yml`, `collection.yml` and a `collections/` tree of
target directories below it. The generated `server.yml` already contains the
same DSN, so the server command below needs no DSN flag.

With the default seed (`1`), the dataset contains:

| Item | Count |
|------|-------|
| Orgs | 3 (`default`, `lab`, `ops`) |
| Collections | 24 (5–10 per org) |
| Playbooks | 62 (1–5 per collection) |
| Hosts | 5,558 distinct (5–1000 per collection, min/max always present) |
| Past runs | 215, of which 59 failed (`exit_code=2`, with failed hosts) |
| Host recaps / task results | 22,738 / ~6,000 |
| UI accounts | 3 |

Runs are dated in the past few days and include Ansible-style PLAY RECAP
output, changed/failed host counters, facts and task history.

### Test accounts

Each account's password is the same as its login. Grants are per collection,
so the visible orgs differ:

| Login / password | Access |
|------------------|--------|
| `admin` / `admin` | all 3 orgs (every collection) |
| `one-org` / `one-org` | `default` only |
| `two-orgs` / `two-orgs` | `default` and `lab` |

These are throwaway test credentials — do not reuse the passwords anywhere
else. With `-orgs 2`, `two-orgs` covers both orgs; with `-orgs 1`, both limited
accounts see the only org.

The generated configuration deliberately uses `schedule: "@every 87600h"`
(10 years) so the scheduler does not start new runs and overwrite the fixture.
Manual triggers from the UI/API would still work and would create new runs;
avoid them if you want the dataset to stay pristine.

### Useful flags

```sh
go run ./tests/seed -h
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-dsn` | `strangle-seed.db` | PostgreSQL DSN or SQLite file path |
| `-config-dir` | *(none)* | Also emit the matching YAML config tree |
| `-reset` | `false` | Delete an existing SQLite file first (SQLite only) |
| `-seed` | `1` | Random seed; the same value reproduces the same dataset |
| `-orgs` | `3` | Number of orgs (includes `default`) |
| `-collections-min/max` | `5` / `10` | Collections per org |
| `-playbooks-min/max` | `1` / `5` | Playbooks per collection |
| `-hosts-min/max` | `5` / `1000` | Configured hosts per collection |
| `-runs-min/max` | `2` / `5` | Past runs per playbook |
| `-failure-rate` | `0.25` | Probability that an extra run fails |

Each org always gets at least one failed run even with `-failure-rate 0`.

## 3. Run the server with the dataset

```sh
go run ./cmd/strangle-server serve \
  -s /tmp/opencode/strangle-seed-config/server.yml \
  -c /tmp/opencode/strangle-seed-config/collection.yml
```

Then open <http://localhost:8080>. The seeded accounts turn UI authentication
on: log in as `admin` / `admin` (or `one-org`, `two-orgs` with password equal to
the login) to compare the different access levels. The API is under `/api/v1`,
Swagger UI under `/api/docs`.

For API checks, log in with a cookie jar first:

```sh
curl -s -c /tmp/strangle-cookies -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin"}' \
  localhost:8080/api/v1/login
curl -s -b /tmp/strangle-cookies localhost:8080/api/v1/orgs | python3 -m json.tool
curl -s -b /tmp/strangle-cookies localhost:8080/api/v1/orgs/lab/collections | python3 -m json.tool
curl -s -b /tmp/strangle-cookies \
  localhost:8080/api/v1/orgs/default/collections/default-c01/hosts | python3 -m json.tool
```

Repeat the `/orgs` call with `one-org` / `one-org` or `two-orgs` / `two-orgs` to
see only their orgs.

Browse one host's past runs through the playbook-scoped endpoint:

```
/api/v1/orgs/{org}/collections/{collection}/playbooks/{playbook}/hosts/{host}/runs
```

For example, with the default seed:
`curl -s -b /tmp/strangle-cookies localhost:8080/api/v1/orgs/default/collections/default-c01/playbooks/web/hosts/default-c01-h0001/runs`.

## SQLite variant (inspection and tests only)

```sh
go run ./tests/seed -dsn /tmp/opencode/strangle-seed.db -reset
sqlite3 /tmp/opencode/strangle-seed.db \
  "select count(*) from runs; select count(*) from runs where success = 0;"
```

`-reset` deletes the SQLite file first; for PostgreSQL, drop/recreate the
database instead. The generator refuses a non-empty database, so it cannot
accidentally add a second dataset to an existing one.

## Tests

```sh
go test ./tests/seed/... -count=1
```

The tests check the requested ranges and reproducibility of the plan, that a
seed populates a store with both failed and successful runs, that the three
accounts have password == login and the expected collection grants, and that
the emitted config loads with `cfg.Load` and matches the seeded
orgs/collections/playbooks.
