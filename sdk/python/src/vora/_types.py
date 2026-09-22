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
from typing import Any, Literal, Mapping, Sequence

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
    memory_peak_bytes: int | None = None
    egress_allowed: int | None = None
    egress_denied: int | None = None


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
class ExecChunk:
    """One unit of streamed exec output."""

    kind: Literal["stdout", "stderr", "exit"]
    data: bytes = b""
    exit_code: int | None = None
    reason: str | None = None


@dataclass(frozen=True)
class ExecResult:
    stdout: str
    stderr: str
    exit_code: int
    reason: str | None = None


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
        return ExecChunk("exit", exit_code=_int(exit_.get("exitCode")) or 0, reason=_string(exit_.get("reason")))
    return None
