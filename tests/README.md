# Test and demo configs

This directory holds every example configuration and Ansible playbook used for
testing or demonstrating strangle. The live defaults the binaries load are in
[`../config`](../config) (`server.yml` and `collection.yml`).

- [`config/strangle.yml`](config/strangle.yml) — documented reference for a
  per-target `strangle.yml`.
- [`seed/`](seed/README.md) — deterministic generator for a large test dataset
  (orgs, collections, playbooks, hosts and past runs, some failed), with
  instructions for creating the database and running the server against it.
- [`demo/`](demo) — self-contained target that runs a playbook against
  localhost. Add it to `collection.yml` as a target pointing at
  `tests/demo` to run it.
- [`ssh/`](ssh) — inventory and playbook for the Go-managed SSH transport;
  see [`../docs/ssh-transport.md`](../docs/ssh-transport.md).
- [`sandbox/validate/`](sandbox/validate) — collection and playbook that
  verify Landlock isolation; see its [README](sandbox/validate/README.md).
