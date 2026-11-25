# Documentation

- [Architecture](architecture.md) — the master/worker process model, the
  JSON-RPC worker protocol, worker supervision, the job queue, the store and
  shutdown.
- [Configuration](configuration.md) — full reference for `server.yml`,
  `collection.yml` and the per-target `strangle.yml`, including per-job
  schedules, the mandatory PostgreSQL DSN and the Go-managed SSH transport options.
- [API](api.md) — the `/api/v1` endpoints with request and response examples,
  plus the served OpenAPI 3.1 document at `/api/v1/openapi.json` (generated
  with swaggo/swag v2) and Swagger UI at `/api/docs`.
- [Git-backed collections](git-collections.md) — upstream repositories, the
  collection `hook` webhook, branch switching and template variables.
- [SSH transport](ssh-transport.md) — the Go-managed persistent SSH
  transport: the `strangle_ssh` connection plugin, connection pooling,
  authentication and host-key approval through the playbook-scoped host
  endpoints.
