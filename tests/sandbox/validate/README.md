# Validating collection

This is a small Ansible collection that checks Landlock isolation from inside
a sandboxed worker. It is **not** enabled in `config/collection.yml` by
default; add it as a target when you want to verify a sandbox deployment:

```yaml
collections:
  - name: home                       # an ordinary collection
    path: /path/to/home
  - name: sandbox-validate
    path: /path/to/strangle/tests/sandbox/validate
```

Enable sandboxing in `server.yml`:

```yaml
sandbox:
  enabled: true
```

Then start the master with an absolute path to a file in *another*
collection that the validating playbook must not be able to read:

```sh
STRANGLE_VALIDATE_NEIGHBOR=/path/to/home/secrets.txt \
  strangle-server serve -s config/server.yml -c config/collection.yml
```

Wait for the scheduler to run the `isolation` playbook of `sandbox-validate`
(there is no HTTP endpoint to trigger a run). It asserts that:

- the validating collection can read its own `secret.txt`;
- `STRANGLE_VALIDATE_NEIGHBOR` is not readable;
- the collection checkout cannot be written to;
- the worker's private scratch directory (`ANSIBLE_LOCAL_TEMP`) is writable.

If any assertion fails, the sandbox is not isolating collections as
expected. Check the master log for the "Landlock is not supported by this
kernel" warning: without Landlock the worker still runs, but no isolation is
enforced.

The same checks are exercised automatically by
`sandbox.TestLandlockIsolation` when the kernel supports Landlock.
