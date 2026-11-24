# strangle

[![CI](https://github.com/kakwa/strangle/actions/workflows/ci.yml/badge.svg)](https://github.com/kakwa/strangle/actions/workflows/ci.yml)

Strangle: a server that applies to job postings periodically and reports on the results.

## Layout

```
cmd/strangle/        entrypoint: config, wiring, graceful shutdown
internal/
  config/            configuration from environment variables
  applier/           job + report types and the Applier interface (stub for now)
  scheduler/         runs the applier over the job list on an interval
  store/             in-memory, bounded store of reports
  server/            HTTP API + serves the built web UI
web/                 React (Vite) web UI, styled with Mantine
```

## API

- `GET /api/health`   — liveness
- `GET /api/jobs`     — configured job postings
- `GET /api/reports`  — application reports, newest first

## Configuration

| Variable            | Default       | Description                       |
|---------------------|---------------|-----------------------------------|
| `STRANGLE_ADDR`     | `:8080`       | HTTP listen address               |
| `STRANGLE_INTERVAL` | `15m`         | How often jobs are applied to     |

## Running

Requires Go and Node.js.

```sh
# build and start the server
make build
STRANGLE_INTERVAL=1m ./bin/strangle

# web UI (development): Vite dev server on :5173, proxies /api to :8080
make web-dev

# web UI (production): builds into web/dist, served by the Go server
make web
```

## Status

Skeleton — the applier is a stub and the store is in-memory.
