# Documentation

- [Architecture](architecture.md) — the master/worker process model, the
  gRPC runner service, worker supervision, the job queue, the store and
  shutdown.
- [Configuration](configuration.md) — full reference for `server.yml`,
  `collection.yml` and the per-target `strangle.yml`, including per-job
  schedules, failure notifications and the Go-managed SSH transport options.
- [API](api.md) — every HTTP endpoint with request and response examples.
- [Git-backed collections](git-collections.md) — upstream repositories, the
  `POST /api/sync` webhook, branch switching and template variables.
- [SSH transport](ssh-transport.md) — the Go-managed persistent SSH
  transport: the `strangle_ssh` connection plugin, connection pooling,
  authentication and host-key policies.
