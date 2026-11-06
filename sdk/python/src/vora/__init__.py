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
from ._errors import (
    ConflictError,
    GatewayTimeoutError,
    PoolDrainingError,
    PoolNotLoadedError,
    PoolNotOnRunnerError,
    PoolRunError,
    PoolSpecConflictError,
    RateLimitedError,
    RequestTooLargeError,
    RetryLaterError,
    UnavailableError,
    VoraError,
)
from ._http import RetryPolicy
from ._pools import Pool, Pools, RunStream
from ._sandbox import Sandbox
from ._snapshots import Snapshot, Snapshots
from ._types import (
    ExecChunk,
    ExecResult,
    ExecUsage,
    NetworkPolicy,
    PoolInfo,
    PoolReuse,
    PoolResetOutcome,
    PoolRun,
    PoolRunDone,
    PoolRunResult,
    PoolSpec,
    PoolState,
    PoolStats,
    ResourceLimits,
    RunChunk,
    RunExit,
    RunStreamError,
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
    "ConflictError",
    "ExecChunk",
    "ExecResult",
    "ExecUsage",
    "GatewayTimeoutError",
    "NetworkPolicy",
    "Pool",
    "PoolDrainingError",
    "PoolInfo",
    "PoolNotLoadedError",
    "PoolNotOnRunnerError",
    "PoolResetOutcome",
    "PoolReuse",
    "PoolRun",
    "PoolRunDone",
    "PoolRunError",
    "PoolRunResult",
    "PoolSpec",
    "PoolSpecConflictError",
    "PoolState",
    "PoolStats",
    "Pools",
    "RateLimitedError",
    "RequestTooLargeError",
    "ResourceLimits",
    "RetryLaterError",
    "RetryPolicy",
    "RunChunk",
    "RunExit",
    "RunStream",
    "RunStreamError",
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
    "UnavailableError",
    "Vora",
    "VoraError",
    "parse_cpu",
    "parse_duration",
    "parse_size",
]
