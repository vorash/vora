"""
Python client for the Vora sandbox API.

```python
import vora

async with vora.Vora() as client:
    sandbox = await client.sandboxes.create(image="vora/python", memory="512Mi")
    result = await sandbox.exec("python -c 'print(45)'")
    print(result.stdout)
    await sandbox.destroy()
```
"""

from ._client import Vora, Sandboxes
from ._errors import VoraError
from ._sandbox import Sandbox
from ._snapshots import Snapshot, Snapshots
from ._types import (
    ExecChunk,
    ExecResult,
    ExecUsage,
    NetworkPolicy,
    ResourceLimits,
    SandboxEgress,
    SandboxEvent,
    SandboxExec,
    SandboxInfo,
    SandboxSpec,
    SnapshotInfo,
    Stats,
)
from ._units import parse_cpu, parse_duration, parse_size

__all__ = [
    "ExecChunk",
    "ExecResult",
    "ExecUsage",
    "NetworkPolicy",
    "ResourceLimits",
    "Sandbox",
    "SandboxEgress",
    "SandboxEvent",
    "SandboxExec",
    "SandboxInfo",
    "SandboxSpec",
    "Sandboxes",
    "Snapshot",
    "SnapshotInfo",
    "Snapshots",
    "Stats",
    "Vora",
    "VoraError",
    "parse_cpu",
    "parse_duration",
    "parse_size",
]
