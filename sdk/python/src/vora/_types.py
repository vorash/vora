"""Typed values and the vora-api wire codec.

The vora-api speaks protojson: camelCase fields, enums as their proto names,
bytes as base64, and 64-bit integers as *strings* (protojson emits them that
way to survive JSON's float precision). This module is the single place that
knows those rules — everything above it works with plain values. It mirrors
``sdk/typescript/src/types.ts`` field for field.
"""

from __future__ import annotations

import base64
from dataclasses import dataclass
from typing import Any, Literal, Mapping, Sequence, cast

from ._errors import VoraError
from ._units import parse_cpu, parse_duration, parse_size

Json = dict[str, Any]

NETWORK_SHORTHANDS: dict[str, str] = {
    "none": "NETWORK_NONE",
    "restricted": "NETWORK_RESTRICTED",
    "full": "NETWORK_FULL",
    "private": "NETWORK_PRIVATE",
}

ExitReason = Literal[
    "EXITED",
    "OOM_KILLED",
    "PIDS_LIMIT_EXCEEDED",
    "TIMEOUT_EXCEEDED",
    "POLICY_VIOLATION",
    "RUNTIME_FAILURE",
    "DISK_QUOTA_EXCEEDED",
]


PoolReuse = Literal["POOL_REUSE_RESET", "POOL_REUSE_UNSAFE_NO_RESET"]
PoolState = Literal["POOL_ACTIVE", "POOL_DRAINING", "POOL_NOT_LOADED"]
PoolResetOutcome = Literal["POOL_RESET_OK", "POOL_RESET_FAILED", "POOL_RESET_SKIPPED", "POOL_RESET_RETIRED"]

REUSE_SHORTHANDS: dict[str, str] = {
    "reset": "POOL_REUSE_RESET",
    "unsafe_no_reset": "POOL_REUSE_UNSAFE_NO_RESET",
}


@dataclass(frozen=True)
class ResourceLimits:
    cpu_millis: int | None = None
    memory_bytes: int | None = None
    pids: int | None = None
    disk_bytes: int | None = None


@dataclass(frozen=True)
class NetworkPolicy:
    mode: str | None = None
    allowed_domains: tuple[str, ...] | None = None
    broker_aliases: tuple[str, ...] | None = None


@dataclass(frozen=True)
class SandboxSpec:
    sandbox_id: str | None = None
    tenant_id: str | None = None
    agent_id: str | None = None
    image: str | None = None
    backend: str | None = None
    env: tuple[str, ...] | None = None
    ttl_seconds: int | None = None
    resources: ResourceLimits | None = None
    network: NetworkPolicy | None = None


@dataclass(frozen=True)
class SandboxInfo:
    id: str
    spec: SandboxSpec | None = None
    state: str | None = None
    runner_id: str | None = None
    reason: str | None = None
    created_at_unix_ms: int | None = None
    updated_at_unix_ms: int | None = None
    image_rootfs: str | None = None
    pool_id: str | None = None


@dataclass(frozen=True)
class Stats:
    memory_bytes: int | None = None
    memory_peak_bytes: int | None = None
    cpu_millis: int | None = None
    pids: int | None = None
    oom_kills: int | None = None
    pids_limit_hits: int | None = None


@dataclass(frozen=True)
class SandboxEvent:
    sandbox_id: str | None = None
    from_state: str | None = None
    to_state: str | None = None
    reason: str | None = None
    runner_id: str | None = None
    timestamp_unix_ms: int | None = None


@dataclass(frozen=True)
class SandboxExec:
    execution_id: str | None = None
    command: str | None = None
    args: tuple[str, ...] | None = None
    cwd: str | None = None
    started_at_unix_ms: int | None = None
    finished_at_unix_ms: int | None = None
    exit_code: int | None = None
    reason: str | None = None
    stdout_bytes: int | None = None
    stderr_bytes: int | None = None
    cpu_millis: int | None = None
    # The peak during this exec's window; memory_peak_scope says whether the
    # kernel scoped it to the window ("PEAK_SCOPE_WINDOW") or it is the
    # sandbox's lifetime peak ("PEAK_SCOPE_LIFETIME").
    memory_peak_bytes: int | None = None
    egress_allowed: int | None = None
    egress_denied: int | None = None
    memory_peak_scope: str | None = None
    oom_kills: int | None = None
    pids_limit_hits: int | None = None
    pool_id: str | None = None
    sandbox_run: int | None = None


@dataclass(frozen=True)
class SandboxEgress:
    sandbox_id: str | None = None
    host: str | None = None
    remote: str | None = None
    allowed: bool | None = None
    reason: str | None = None
    timestamp_unix_ms: int | None = None
    alias: str | None = None


@dataclass(frozen=True)
class SnapshotInfo:
    id: str
    sandbox_id: str | None = None
    tenant_id: str | None = None
    runner_id: str | None = None
    spec: SandboxSpec | None = None
    bytes: int | None = None
    created_at_unix_ms: int | None = None


@dataclass(frozen=True)
class ExecUsage:
    """What the sandbox used during one exec.

    The counters are per-exec deltas. The window is the whole sandbox, so
    concurrent execs in one sandbox have overlapping windows and count each
    other's usage. ``memory_peak_scope`` is ``"PEAK_SCOPE_WINDOW"`` when the
    peak covers only this exec, ``"PEAK_SCOPE_LIFETIME"`` when the kernel
    could not reset it (before Linux 6.12), and None when it was not measured.
    """

    cpu_millis: int | None = None
    memory_peak_bytes: int | None = None
    memory_peak_scope: str | None = None
    oom_kills: int | None = None
    pids_limit_hits: int | None = None


@dataclass(frozen=True)
class ExecChunk:
    """One unit of streamed exec output.

    An ``exit`` chunk also carries the exec's audit record id and its usage.
    """

    kind: Literal["stdout", "stderr", "exit"]
    data: bytes = b""
    exit_code: int | None = None
    reason: str | None = None
    execution_id: str | None = None
    usage: ExecUsage | None = None


@dataclass(frozen=True)
class ExecResult:
    stdout: str
    stderr: str
    exit_code: int
    reason: str | None = None


@dataclass(frozen=True)
class PoolSpec:
    """A pool's definition. Unset limits are None until the runner fills in defaults."""

    name: str | None = None
    tenant_id: str | None = None
    agent_id: str | None = None
    sandbox: SandboxSpec | None = None
    size: int | None = None
    max_size: int | None = None
    max_runs: int | None = None
    max_age_seconds: int | None = None
    max_run_seconds: int | None = None
    borrow_timeout_ms: int | None = None
    idle_expiry_seconds: int | None = None
    reuse: PoolReuse | None = None


@dataclass(frozen=True)
class PoolStats:
    """A runner's snapshot of one pool. Counters reset when the runner restarts.

    These are ints, not ``int | None``: the runner fills all of them from memory on
    every answer, so a key protojson left out is a zero the runner did report.
    """

    starting: int = 0
    idle: int = 0
    busy: int = 0
    resetting: int = 0
    retiring: int = 0
    runs_total: int = 0
    warm_runs_total: int = 0
    resets_total: int = 0
    reset_failures_total: int = 0
    destroyed_total: int = 0
    exhausted_total: int = 0
    start_failed_total: int = 0
    last_start_error: str | None = None
    dormant: bool = False


@dataclass(frozen=True)
class PoolInfo:
    """A pool's record.

    Read ``state`` before ``stats``. In a listing, a pool whose runner did not answer
    has ``state`` and ``stats`` both None. A ``POOL_NOT_LOADED`` pool has all-zero
    stats, and ``not_loaded_reason`` says why it has no members.
    """

    id: str
    spec: PoolSpec | None = None
    state: PoolState | None = None
    runner_id: str | None = None
    stats: PoolStats | None = None
    created_at_unix_ms: int | None = None
    not_loaded_reason: str | None = None


@dataclass(frozen=True)
class PoolRun:
    """How a run got its sandbox: the first frame of every pool run.

    protojson leaves out zeros, so a None duration is either not measured or under
    a millisecond, and the API does not say which. ``reset_ms`` here is the reset
    that prepared this sandbox, None on its first run. This run's own reset is
    ``PoolRunDone.reset_ms``.
    """

    pool_id: str | None = None
    sandbox_id: str | None = None
    execution_id: str | None = None
    warm: bool = False
    startup_ms: int | None = None
    reset_ms: int | None = None
    sandbox_run: int | None = None
    reuse: PoolReuse | None = None


@dataclass(frozen=True)
class RunExit:
    """How the command ended. ``usage`` fields stay None when not reported."""

    exit_code: int = 0
    reason: str | None = None
    execution_id: str | None = None
    usage: ExecUsage | None = None


@dataclass(frozen=True)
class PoolRunDone:
    """What happened to the sandbox after the run.

    ``outcome`` is ``POOL_RESET_FAILED`` when a reset could not prove the sandbox
    clean, and ``POOL_RESET_SKIPPED`` on an unsafe pool, where nothing was cleaned.
    ``retire_cause`` is set when the sandbox is being destroyed.
    """

    outcome: PoolResetOutcome | None = None
    reset_ms: int | None = None
    retire_cause: str | None = None


@dataclass(frozen=True)
class RunStreamError:
    """Why a stream ended after ``exit`` with no ``run_done``."""

    code: str
    message: str


@dataclass(frozen=True)
class RunChunk:
    """One unit of a pool run: ``run``, output, ``exit``, then ``run_done``.

    ``stream_error`` comes only after ``exit``, in place of ``run_done``: the run's
    result stands, and the reset's outcome is unknown. A failure before ``exit``
    raises ``PoolRunError`` instead.
    """

    kind: Literal["run", "stdout", "stderr", "exit", "run_done", "stream_error"]
    data: bytes = b""
    run: PoolRun | None = None
    exit: RunExit | None = None
    done: PoolRunDone | None = None
    error: RunStreamError | None = None


@dataclass(frozen=True)
class PoolRunResult:
    """A finished pool run.

    ``done`` is None when the stream ended before the reset's outcome arrived, and
    ``stream_error`` says why. The result is still valid: the command had finished.
    """

    run: PoolRun
    exit: RunExit
    stdout: str
    stderr: str
    done: PoolRunDone | None = None
    stream_error: RunStreamError | None = None


# ---------------------------------------------------------------------------
# Encoders: SDK values -> protojson wire shape.
# ---------------------------------------------------------------------------


def encode_sandbox_spec(options: Mapping[str, Any]) -> Json:
    image = options.get("image")
    if not isinstance(image, str) or image == "":
        raise VoraError("create: image is required")
    spec: Json = {"image": image}
    for wire, key in (("backend", "backend"), ("agentId", "agent_id"), ("tenantId", "tenant_id")):
        if options.get(key) is not None:
            spec[wire] = options[key]

    resources: Json = {}
    if options.get("cpu") is not None:
        resources["cpuMillis"] = str(parse_cpu(options["cpu"]))
    if options.get("memory") is not None:
        resources["memoryBytes"] = str(parse_size(options["memory"]))
    if options.get("disk") is not None:
        resources["diskBytes"] = str(parse_size(options["disk"]))
    if options.get("pids") is not None:
        resources["pids"] = str(int(options["pids"]))
    if resources:
        spec["resources"] = resources

    if options.get("network") is not None:
        spec["network"] = _encode_network(options["network"])
    if options.get("env") is not None:
        spec["env"] = _encode_env(options["env"])
    if options.get("ttl") is not None:
        spec["ttlSeconds"] = str(parse_duration(options["ttl"]))
    return spec


def _encode_network(network: str | NetworkPolicy | Mapping[str, Any]) -> Json:
    if isinstance(network, str):
        return {"mode": NETWORK_SHORTHANDS.get(network, network)}
    policy = network if isinstance(network, Mapping) else {
        "mode": network.mode,
        "allowed_domains": network.allowed_domains,
        "broker_aliases": network.broker_aliases,
    }
    out: Json = {}
    mode = policy.get("mode")
    if isinstance(mode, str):
        out["mode"] = NETWORK_SHORTHANDS.get(mode, mode)
    domains = policy.get("allowed_domains") or ()
    if domains:
        out["allowedDomains"] = list(domains)
    aliases = policy.get("broker_aliases") or ()
    if aliases:
        out["brokerAliases"] = list(aliases)
    return out


def _encode_env(env: Sequence[str] | Mapping[str, str]) -> list[str]:
    if isinstance(env, Mapping):
        return [f"{key}={value}" for key, value in env.items()]
    return list(env)


def encode_exec_request(options: Mapping[str, Any]) -> Json:
    command = options.get("command")
    if not isinstance(command, str) or command == "":
        raise VoraError("exec: command is required")
    request: Json = {"command": command}
    args = options.get("args") or ()
    if args:
        request["args"] = list(args)
    if options.get("cwd") is not None:
        request["cwd"] = options["cwd"]
    env = options.get("env") or ()
    if env:
        request["env"] = list(env)
    if options.get("timeout_seconds") is not None:
        request["timeoutSeconds"] = str(int(options["timeout_seconds"]))
    if options.get("stdin") is not None:
        request["stdin"] = encode_base64(options["stdin"])
    return request


def encode_pool_spec(name: str, options: Mapping[str, Any]) -> Json:
    """The ``uint32`` limits go as JSON numbers, the ``int64`` ones as strings."""
    spec: Json = {"name": name, "sandbox": encode_sandbox_spec(options)}
    for wire, key in (("tenantId", "tenant_id"), ("agentId", "agent_id")):
        if options.get(key) is not None:
            spec[wire] = options[key]
    for wire, key in (("size", "size"), ("maxSize", "max_size"), ("maxRuns", "max_runs")):
        if options.get(key) is not None:
            spec[wire] = int(options[key])
    for wire, key in (
        ("maxAgeSeconds", "max_age_seconds"),
        ("maxRunSeconds", "max_run_seconds"),
        ("borrowTimeoutMs", "borrow_timeout_ms"),
        ("idleExpirySeconds", "idle_expiry_seconds"),
    ):
        if options.get(key) is not None:
            spec[wire] = str(int(options[key]))
    reuse = options.get("reuse")
    if reuse is not None:
        spec["reuse"] = REUSE_SHORTHANDS.get(reuse, reuse)
    return spec


def encode_base64(value: str | bytes) -> str:
    return base64.b64encode(value.encode("utf-8") if isinstance(value, str) else value).decode("ascii")


def decode_base64(value: str) -> bytes:
    return base64.b64decode(value)


# ---------------------------------------------------------------------------
# Decoders: protojson wire shape -> plain values.
# ---------------------------------------------------------------------------


def _string(value: Any) -> str | None:
    return value if isinstance(value, str) else None


def _bool(value: Any) -> bool | None:
    return value if isinstance(value, bool) else None


def _int(value: Any) -> int | None:
    """protojson writes int64 as a string; a number is also accepted on input."""
    if value is None or value == "":
        return None
    try:
        return int(value)
    except (TypeError, ValueError):
        return None


def _object(value: Any) -> Json | None:
    return value if isinstance(value, dict) else None


def _strings(value: Any) -> tuple[str, ...] | None:
    if not isinstance(value, list):
        return None
    return tuple(item for item in value if isinstance(item, str))


def decode_sandbox_spec(raw: Any) -> SandboxSpec | None:
    source = _object(raw)
    if source is None:
        return None
    resources = _object(source.get("resources"))
    limits = None
    if resources is not None:
        limits = ResourceLimits(
            cpu_millis=_int(resources.get("cpuMillis")),
            memory_bytes=_int(resources.get("memoryBytes")),
            pids=_int(resources.get("pids")),
            disk_bytes=_int(resources.get("diskBytes")),
        )
    network = _object(source.get("network"))
    policy = None
    if network is not None:
        policy = NetworkPolicy(
            mode=_string(network.get("mode")),
            allowed_domains=_strings(network.get("allowedDomains")),
            broker_aliases=_strings(network.get("brokerAliases")),
        )
    return SandboxSpec(
        sandbox_id=_string(source.get("sandboxId")),
        tenant_id=_string(source.get("tenantId")),
        agent_id=_string(source.get("agentId")),
        image=_string(source.get("image")),
        backend=_string(source.get("backend")),
        env=_strings(source.get("env")),
        ttl_seconds=_int(source.get("ttlSeconds")),
        resources=limits,
        network=policy,
    )


def decode_sandbox_info(raw: Any) -> SandboxInfo:
    source = _object(raw) or {}
    return SandboxInfo(
        id=_string(source.get("id")) or "",
        spec=decode_sandbox_spec(source.get("spec")),
        state=_string(source.get("state")),
        runner_id=_string(source.get("runnerId")),
        reason=_string(source.get("reason")),
        created_at_unix_ms=_int(source.get("createdAtUnixMs")),
        updated_at_unix_ms=_int(source.get("updatedAtUnixMs")),
        image_rootfs=_string(source.get("imageRootfs")),
        pool_id=_string(source.get("poolId")),
    )


def decode_stats(raw: Any) -> Stats:
    source = _object(raw) or {}
    return Stats(
        memory_bytes=_int(source.get("memoryBytes")),
        memory_peak_bytes=_int(source.get("memoryPeakBytes")),
        cpu_millis=_int(source.get("cpuMillis")),
        pids=_int(source.get("pids")),
        oom_kills=_int(source.get("oomKills")),
        pids_limit_hits=_int(source.get("pidsLimitHits")),
    )


def decode_sandbox_event(raw: Any) -> SandboxEvent:
    source = _object(raw) or {}
    return SandboxEvent(
        sandbox_id=_string(source.get("sandboxId")),
        from_state=_string(source.get("from")),
        to_state=_string(source.get("to")),
        reason=_string(source.get("reason")),
        runner_id=_string(source.get("runnerId")),
        timestamp_unix_ms=_int(source.get("timestampUnixMs")),
    )


def decode_sandbox_exec(raw: Any) -> SandboxExec:
    source = _object(raw) or {}
    return SandboxExec(
        execution_id=_string(source.get("executionId")),
        command=_string(source.get("command")),
        args=_strings(source.get("args")),
        cwd=_string(source.get("cwd")),
        started_at_unix_ms=_int(source.get("startedAtUnixMs")),
        finished_at_unix_ms=_int(source.get("finishedAtUnixMs")),
        exit_code=_int(source.get("exitCode")),
        reason=_string(source.get("reason")),
        stdout_bytes=_int(source.get("stdoutBytes")),
        stderr_bytes=_int(source.get("stderrBytes")),
        cpu_millis=_int(source.get("cpuMillis")),
        memory_peak_bytes=_int(source.get("memoryPeakBytes")),
        egress_allowed=_int(source.get("egressAllowed")),
        egress_denied=_int(source.get("egressDenied")),
        memory_peak_scope=_string(source.get("memoryPeakScope")),
        oom_kills=_int(source.get("oomKills")),
        pids_limit_hits=_int(source.get("pidsLimitHits")),
        pool_id=_string(source.get("poolId")),
        sandbox_run=_int(source.get("sandboxRun")),
    )


def decode_sandbox_egress(raw: Any) -> SandboxEgress:
    source = _object(raw) or {}
    return SandboxEgress(
        sandbox_id=_string(source.get("sandboxId")),
        host=_string(source.get("host")),
        remote=_string(source.get("remote")),
        allowed=_bool(source.get("allowed")),
        reason=_string(source.get("reason")),
        timestamp_unix_ms=_int(source.get("timestampUnixMs")),
        alias=_string(source.get("alias")),
    )


def decode_snapshot_info(raw: Any) -> SnapshotInfo:
    source = _object(raw) or {}
    return SnapshotInfo(
        id=_string(source.get("id")) or "",
        sandbox_id=_string(source.get("sandboxId")),
        tenant_id=_string(source.get("tenantId")),
        runner_id=_string(source.get("runnerId")),
        spec=decode_sandbox_spec(source.get("spec")),
        bytes=_int(source.get("bytes")),
        created_at_unix_ms=_int(source.get("createdAtUnixMs")),
    )


def decode_exec_chunk(raw: Any) -> ExecChunk | None:
    source = _object(raw)
    if source is None:
        return None
    stdout = _string(source.get("stdout"))
    if stdout is not None:
        return ExecChunk("stdout", data=decode_base64(stdout))
    stderr = _string(source.get("stderr"))
    if stderr is not None:
        return ExecChunk("stderr", data=decode_base64(stderr))
    exit_ = _object(source.get("exit"))
    if exit_ is not None:
        return ExecChunk(
            "exit",
            exit_code=_int(exit_.get("exitCode")) or 0,
            reason=_string(exit_.get("reason")),
            execution_id=_string(exit_.get("executionId")),
            usage=decode_exec_usage(exit_.get("usage")),
        )
    return None


def decode_exec_usage(raw: Any) -> ExecUsage | None:
    source = _object(raw)
    if source is None:
        return None
    return ExecUsage(
        cpu_millis=_int(source.get("cpuMillis")),
        memory_peak_bytes=_int(source.get("memoryPeakBytes")),
        memory_peak_scope=_string(source.get("memoryPeakScope")),
        oom_kills=_int(source.get("oomKills")),
        pids_limit_hits=_int(source.get("pidsLimitHits")),
    )


def decode_pool_spec(raw: Any) -> PoolSpec | None:
    source = _object(raw)
    if source is None:
        return None
    return PoolSpec(
        name=_string(source.get("name")),
        tenant_id=_string(source.get("tenantId")),
        agent_id=_string(source.get("agentId")),
        sandbox=decode_sandbox_spec(source.get("sandbox")),
        size=_int(source.get("size")),
        max_size=_int(source.get("maxSize")),
        max_runs=_int(source.get("maxRuns")),
        max_age_seconds=_int(source.get("maxAgeSeconds")),
        max_run_seconds=_int(source.get("maxRunSeconds")),
        borrow_timeout_ms=_int(source.get("borrowTimeoutMs")),
        idle_expiry_seconds=_int(source.get("idleExpirySeconds")),
        reuse=cast("PoolReuse | None", _string(source.get("reuse"))),
    )


def decode_pool_stats(raw: Any) -> PoolStats | None:
    source = _object(raw)
    if source is None:
        return None
    return PoolStats(
        starting=_int(source.get("starting")) or 0,
        idle=_int(source.get("idle")) or 0,
        busy=_int(source.get("busy")) or 0,
        resetting=_int(source.get("resetting")) or 0,
        retiring=_int(source.get("retiring")) or 0,
        runs_total=_int(source.get("runsTotal")) or 0,
        warm_runs_total=_int(source.get("warmRunsTotal")) or 0,
        resets_total=_int(source.get("resetsTotal")) or 0,
        reset_failures_total=_int(source.get("resetFailuresTotal")) or 0,
        destroyed_total=_int(source.get("destroyedTotal")) or 0,
        exhausted_total=_int(source.get("exhaustedTotal")) or 0,
        start_failed_total=_int(source.get("startFailedTotal")) or 0,
        last_start_error=_string(source.get("lastStartError")),
        dormant=_bool(source.get("dormant")) or False,
    )


def decode_pool_info(raw: Any) -> PoolInfo:
    source = _object(raw) or {}
    return PoolInfo(
        id=_string(source.get("id")) or "",
        spec=decode_pool_spec(source.get("spec")),
        state=cast("PoolState | None", _string(source.get("state"))),
        runner_id=_string(source.get("runnerId")),
        stats=decode_pool_stats(source.get("stats")),
        created_at_unix_ms=_int(source.get("createdAtUnixMs")),
        not_loaded_reason=_string(source.get("notLoadedReason")),
    )


def decode_pool_run(source: Json) -> PoolRun:
    return PoolRun(
        pool_id=_string(source.get("poolId")),
        sandbox_id=_string(source.get("sandboxId")),
        execution_id=_string(source.get("executionId")),
        warm=source.get("warm") is True,
        startup_ms=_int(source.get("startupMs")),
        reset_ms=_int(source.get("resetMs")),
        sandbox_run=_int(source.get("sandboxRun")),
        reuse=cast("PoolReuse | None", _string(source.get("reuse"))),
    )


def decode_run_chunk(raw: Any) -> RunChunk | None:
    """One frame of a run stream. The ``{"error": ...}`` last line is not a frame."""
    source = _object(raw)
    if source is None:
        return None
    run = _object(source.get("run"))
    if run is not None:
        return RunChunk("run", run=decode_pool_run(run))
    stdout = _string(source.get("stdout"))
    if stdout is not None:
        return RunChunk("stdout", data=decode_base64(stdout))
    stderr = _string(source.get("stderr"))
    if stderr is not None:
        return RunChunk("stderr", data=decode_base64(stderr))
    exit_ = _object(source.get("exit"))
    if exit_ is not None:
        return RunChunk(
            "exit",
            exit=RunExit(
                # An exit frame's code is always real: protojson leaves out only a zero.
                exit_code=_int(exit_.get("exitCode")) or 0,
                reason=_string(exit_.get("reason")),
                execution_id=_string(exit_.get("executionId")),
                usage=decode_exec_usage(exit_.get("usage")),
            ),
        )
    done = _object(source.get("runDone"))
    if done is not None:
        return RunChunk(
            "run_done",
            done=PoolRunDone(
                outcome=cast("PoolResetOutcome | None", _string(done.get("outcome"))),
                reset_ms=_int(done.get("resetMs")),
                retire_cause=_string(done.get("retireCause")),
            ),
        )
    return None


def decode_run_error(raw: Any) -> RunStreamError | None:
    """The API's last line for a run that failed after its first frame."""
    source = _object(raw)
    error = _object(source.get("error")) if source is not None else None
    if error is None:
        return None
    return RunStreamError(
        code=_string(error.get("code")) or "run_failed",
        message=_string(error.get("message")) or "the run ended without a result",
    )
