"""Ansible callback that keeps each host's latest facts for strangle.

The Go host owns the embedded interpreter, so facts are stored in a plain
Python module instead of being printed into the captured playbook output. The
runner reads that module after every play and hands the JSON blob to the
applier. This works for any task that returns ansible_facts (including the
implicit Gathering Facts task) without enabling verbose output.
"""

from __future__ import annotations

import json
import sys
import types
from typing import Any

from ansible.plugins.callback import CallbackBase

_STORE = 'strangle_facts_store'


def _facts_store() -> types.ModuleType:
    module = sys.modules.get(_STORE)
    if module is None:
        module = types.ModuleType(_STORE)
        module.facts = {}
        sys.modules[_STORE] = module
    return module


class CallbackModule(CallbackBase):
    CALLBACK_VERSION = 2.0
    CALLBACK_TYPE = 'aggregate'
    CALLBACK_NAME = 'strangle_facts'
    CALLBACK_NEEDS_ENABLED = True

    def v2_runner_on_ok(self, result: Any) -> None:
        facts = result._result.get('ansible_facts')
        if not isinstance(facts, dict) or not facts:
            return
        host = getattr(result._host, 'name', None) or result._host.get_name()
        if not host:
            return
        try:
            # Facts regularly contain non-JSON-native values (dates, IPs,
            # AnsibleUnsafeText), so stringify anything unusual.
            serializable = json.loads(json.dumps(facts, default=str, sort_keys=True))
        except (TypeError, ValueError):
            return
        store = _facts_store()
        current = store.facts.get(host, {})
        current.update(serializable)
        store.facts[host] = current
