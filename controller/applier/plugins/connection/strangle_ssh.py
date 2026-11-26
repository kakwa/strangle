"""Ansible connection plugin backed by the strangle Go SSH pool.

The plugin speaks a small JSON-lines protocol to the Go process over inherited
pipes. Ansible forks worker processes, so this plugin deliberately never calls
back into cgo; the Go side owns all SSH connections and reuses them.
"""

from __future__ import annotations

import base64
import builtins
import fcntl
import json
import os
from typing import Any

from ansible.errors import AnsibleConnectionFailure, AnsibleError
from ansible.plugins.connection import ConnectionBase

DOCUMENTATION = """
    name: strangle_ssh
    short_description: SSH transport managed by the strangle Go process
    description:
        - Uses persistent SSH connections owned by the Go host process.
    author: strangle
    options: {}
"""

_BRIDGE: _Bridge | None = None


class _Bridge:
    def __init__(self) -> None:
        fds = getattr(builtins, "_strangle_ssh_fds", None)
        if not fds:
            raise AnsibleError("strangle SSH bridge is not initialized")

        self._req = os.fdopen(fds["req"], "wb", buffering=0)
        self._resp = os.fdopen(fds["resp"], "rb", buffering=0)
        # Open a fresh file description so flock serializes across forked
        # workers (an inherited fd would share a single lock).
        self._lock = open(fds["lock_path"], "rb", buffering=0)

    def call(self, op: str, **params: Any) -> dict[str, Any]:
        payload = {"id": os.getpid(), "op": op, **params}
        fcntl.flock(self._lock.fileno(), fcntl.LOCK_EX)
        try:
            self._req.write((json.dumps(payload) + "\n").encode("utf-8"))
            line = self._resp.readline()
            if not line:
                raise AnsibleError("strangle SSH bridge closed unexpectedly")
            result = json.loads(line)
            if not result.get("ok"):
                raise AnsibleConnectionFailure(result.get("error", "SSH request failed"))
            parsed = result.get("result")
            return json.loads(parsed) if isinstance(parsed, str) else (parsed or {})
        finally:
            fcntl.flock(self._lock.fileno(), fcntl.LOCK_UN)


def _bridge() -> _Bridge:
    global _BRIDGE
    if _BRIDGE is None:
        _BRIDGE = _Bridge()
    return _BRIDGE


class Connection(ConnectionBase):
    """SSH connection implemented by the Go process."""

    transport = "strangle_ssh"
    has_pipelining = True
    force_persistence = False

    def __init__(self, *args: Any, **kwargs: Any) -> None:
        super().__init__(*args, **kwargs)
        self.host = self._play_context.remote_addr
        self.port = self._play_context.port
        self.user = self._play_context.remote_user

    def _endpoint(self) -> dict[str, Any]:
        return {"host": self.host, "port": self.port or 0, "user": self.user or ""}

    def _connect(self) -> Connection:
        self._connected = True
        return self

    def exec_command(
        self, cmd: str, in_data: bytes | None = None, sudoable: bool = True
    ) -> tuple[int, bytes, bytes]:
        result = _bridge().call(
            "exec",
            endpoint=self._endpoint(),
            command=cmd,
            stdin=base64.b64encode(in_data or b"").decode("ascii"),
        )
        return (
            int(result.get("return_code", 0)),
            base64.b64decode(result.get("stdout", "")),
            base64.b64decode(result.get("stderr", "")),
        )

    def put_file(self, in_path: str, out_path: str) -> None:
        with open(in_path, "rb") as source:
            content = source.read()
        mode = os.stat(in_path).st_mode & 0o7777
        _bridge().call(
            "put",
            endpoint=self._endpoint(),
            path=out_path,
            content=base64.b64encode(content).decode("ascii"),
            mode=mode,
        )

    def fetch_file(self, in_path: str, out_path: str) -> None:
        result = _bridge().call("fetch", endpoint=self._endpoint(), path=in_path)
        with open(out_path, "wb") as destination:
            destination.write(base64.b64decode(result.get("content", "")))

    def close(self) -> None:
        # Connections are kept open in the Go pool and reused across tasks.
        self._connected = False
