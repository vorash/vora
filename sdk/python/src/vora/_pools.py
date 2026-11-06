"""Warm pools: declare a pool, run commands on it, read its stats, delete it."""

from __future__ import annotations

import asyncio
import json
import time
from typing import Any, AsyncGenerator, Sequence
from urllib.parse import urlencode

import httpx

from ._errors import (
    ConflictError,
    GatewayTimeoutError,
    PoolDrainingError,
    PoolNotLoadedError,
    PoolRunError,
    PoolSpecConflictError,
    RateLimitedError,
    RequestTooLargeError,
    UnavailableError,
    VoraError,
)
from ._http import RetryPolicy, Transport, path_segment, retrying
from ._sandbox import exec_request
from ._types import (
    PoolInfo,
    PoolRun,
    PoolRunDone,
    PoolRunResult,
    PoolStats,
    RunChunk,
    RunExit,
    RunStreamError,
    SandboxEvent,
    SandboxExec,
    decode_pool_info,
    decode_run_chunk,
    decode_run_error,
    decode_sandbox_event,
    decode_sandbox_exec,
    encode_pool_spec,
)

# The API reads this much of a body and parses what it got.
_RUN_BODY_LIMIT = 1 << 20

# httpx defaults to 5 s for everything, which is less than any of these calls may
# take. The API's own bounds set each number: 10 s for a plain pool call, 30 s for
# one DELETE, and a run's timeout plus 60 s for its stream.
_CALL_TIMEOUT = httpx.Timeout(30.0)
_DELETE_TIMEOUT = httpx.Timeout(40.0)
_RUN_TAIL_SECONDS = 60.0
_FIRST_FRAME_MARGIN_SECONDS = 30.0
_DRAIN_POLL_SECONDS = 1.0


def _run_timeout(timeout_seconds: int) -> httpx.Timeout:
    # A silent command gives no bytes to read until it exits, so the read timeout has
    # to cover its whole run.
    return httpx.Timeout(30.0, read=timeout_seconds + _RUN_TAIL_SECONDS + _FIRST_FRAME_MARGIN_SECONDS)


def _pool_path(name: str, suffix: str = "", tenant_id: str | None = None) -> str:
    query = f"?{urlencode({'tenant_id': tenant_id})}" if tenant_id is not None else ""
    return f"/v1/pools/{path_segment(name)}{suffix}{query}"


async def _named_conflict(
    transport: Transport,
    name: str,
    tenant_id: str | None,
    error: ConflictError,
    *,
    declaring: bool,
) -> ConflictError:
    """Say which 409 this is from the pool's state, not from the error's words."""
    try:
        info = decode_pool_info(await transport.json(_pool_path(name, tenant_id=tenant_id), timeout=_CALL_TIMEOUT))
    except VoraError:
        return error
    message, status, body = str(error), error.status, error.body
    if info.state == "POOL_DRAINING":
        return PoolDrainingError(message, status=status, body=body)
    if info.state == "POOL_NOT_LOADED":
        return PoolNotLoadedError(message, status=status, body=body, reason=info.not_loaded_reason)
    if declaring and info.state == "POOL_ACTIVE":
        return PoolSpecConflictError(message, status=status, body=body)
    return error


class Pools:
    """Warm pools, reached through ``vora.pools``.

    ``declare``, ``get``, ``list``, ``refresh``, ``stats`` and ``delete`` are safe to
    repeat, so they retry 429 and 503 under the client's ``retry`` policy.
    """

    def __init__(self, transport: Transport, retry: RetryPolicy | None) -> None:
        self._transport = transport
        self._retry = retry

    async def declare(
        self,
        name: str,
        *,
        image: str,
        backend: str | None = None,
        cpu: str | int | float | None = None,
        memory: str | int | None = None,
        pids: int | None = None,
        disk: str | int | None = None,
        network: Any = "none",
        env: Any = None,
        size: int | None = None,
        max_size: int | None = None,
        max_runs: int | None = None,
        max_age_seconds: int | None = None,
        max_run_seconds: int | None = None,
        borrow_timeout_ms: int | None = None,
        idle_expiry_seconds: int | None = None,
        reuse: str | None = None,
        agent_id: str | None = None,
        tenant_id: str | None = None,
    ) -> "Pool":
        """Declare a pool, returning a handle to it. Idempotent for an equal spec.

        ``size=0`` starts every run cold. ``reuse="unsafe_no_reset"`` needs an admin
        key and a runner that grants it. A global admin must pass ``tenant_id``.

        Raises ``PoolSpecConflictError`` when the name has a different spec,
        ``PoolDrainingError`` while a pool of the name is being deleted, and
        ``PoolNotLoadedError`` when the runner refused to load it.
        """
        body = encode_pool_spec(
            name,
            {
                "image": image,
                "backend": backend,
                "cpu": cpu,
                "memory": memory,
                "pids": pids,
                "disk": disk,
                "network": network,
                "env": env,
                "size": size,
                "max_size": max_size,
                "max_runs": max_runs,
                "max_age_seconds": max_age_seconds,
                "max_run_seconds": max_run_seconds,
                "borrow_timeout_ms": borrow_timeout_ms,
                "idle_expiry_seconds": idle_expiry_seconds,
                "reuse": reuse,
                "agent_id": agent_id,
                "tenant_id": tenant_id,
            },
        )
        try:
            raw = await retrying(
                self._retry,
                lambda: self._transport.json(_pool_path(name), method="PUT", json_body=body, timeout=_CALL_TIMEOUT),
            )
        except ConflictError as error:
            raise await _named_conflict(self._transport, name, tenant_id, error, declaring=True) from error
        return Pool(self._transport, self._retry, name, decode_pool_info(raw), tenant_id)

    async def get(self, name: str, *, tenant_id: str | None = None) -> "Pool":
        raw = await retrying(
            self._retry,
            lambda: self._transport.json(_pool_path(name, tenant_id=tenant_id), timeout=_CALL_TIMEOUT),
        )
        return Pool(self._transport, self._retry, name, decode_pool_info(raw), tenant_id)

    async def list(self, *, tenant_id: str | None = None) -> list["Pool"]:
        """Every pool the key may see. Read each ``info.state``: see ``PoolInfo``."""
        query = f"?{urlencode({'tenant_id': tenant_id})}" if tenant_id is not None else ""
        raw = await retrying(
            self._retry,
            lambda: self._transport.json(f"/v1/pools{query}", timeout=_CALL_TIMEOUT),
        )
        pools = []
        for item in raw.get("pools") or []:
            info = decode_pool_info(item)
            name = info.spec.name if info.spec is not None else None
            pools.append(Pool(self._transport, self._retry, name or "", info, tenant_id))
        return pools


class Pool:
    """A handle to one pool. Reached through ``vora.pools``."""

    def __init__(
        self,
        transport: Transport,
        retry: RetryPolicy | None,
        name: str,
        info: PoolInfo,
        tenant_id: str | None,
    ) -> None:
        self._transport = transport
        self._retry = retry
        self._tenant_id = tenant_id
        self._info = info
        self.name = name

    @property
    def id(self) -> str:
        return self._info.id

    @property
    def info(self) -> PoolInfo:
        """The pool's record as of the last call."""
        return self._info

    def _path(self, suffix: str = "") -> str:
        return _pool_path(self.name, suffix, self._tenant_id)

    async def refresh(self) -> PoolInfo:
        raw = await retrying(self._retry, lambda: self._transport.json(self._path(), timeout=_CALL_TIMEOUT))
        self._info = decode_pool_info(raw)
        return self._info

    async def stats(self) -> PoolStats | None:
        """Live stats, or None when the API's answer has none. Read ``info.state`` too."""
        return (await self.refresh()).stats

    async def run(
        self,
        command: str,
        args: Sequence[str] | None = None,
        *,
        timeout_seconds: int,
        cwd: str | None = None,
        env: Sequence[str] | None = None,
        stdin: str | bytes | None = None,
        retry: RetryPolicy | None = None,
    ) -> PoolRunResult:
        """Run a command and collect its output, including what its reset found.

        Takes what ``stream_run`` takes. Raises ``PoolRunError``, with the output so
        far, when the stream fails before ``exit``.
        """
        stdout = bytearray()
        stderr = bytearray()
        run: PoolRun | None = None
        exit_: RunExit | None = None
        done: PoolRunDone | None = None
        stream_error: RunStreamError | None = None
        try:
            async for chunk in self.stream_run(
                command, args, timeout_seconds=timeout_seconds, cwd=cwd, env=env, stdin=stdin, retry=retry
            ):
                if chunk.kind == "stdout":
                    stdout += chunk.data
                elif chunk.kind == "stderr":
                    stderr += chunk.data
                elif chunk.kind == "run":
                    run = chunk.run
                elif chunk.kind == "exit":
                    exit_ = chunk.exit
                elif chunk.kind == "run_done":
                    done = chunk.done
                else:
                    stream_error = chunk.error
        except PoolRunError as error:
            error.stdout = stdout.decode("utf-8", "replace")
            error.stderr = stderr.decode("utf-8", "replace")
            raise
        if run is None or exit_ is None:
            raise PoolRunError("stream_ended", "pool run: the stream had no run frame")
        return PoolRunResult(
            run=run,
            exit=exit_,
            stdout=stdout.decode("utf-8", "replace"),
            stderr=stderr.decode("utf-8", "replace"),
            done=done,
            stream_error=stream_error,
        )

    async def stream_run(
        self,
        command: str,
        args: Sequence[str] | None = None,
        *,
        timeout_seconds: int,
        cwd: str | None = None,
        env: Sequence[str] | None = None,
        stdin: str | bytes | None = None,
        retry: RetryPolicy | None = None,
    ) -> AsyncGenerator[RunChunk, None]:
        """Run a command, yielding ``run``, output, ``exit``, then ``run_done``.

        With ``args=None`` the command is a shell line run via ``/bin/sh -c``. A run
        with ``exit`` is complete; ``run_done`` reports the reset that follows. Leaving
        the loop before ``exit`` cancels the run. A client reading slower than about
        1.5 KiB/s while output is flowing is cut off with ``client_stalled``.

        ``retry`` repeats a 429 or 503, which come before the first frame and mean
        nothing ran. The default is not to retry: a repeated run isn't idempotent, so
        it's the caller's choice. A failure after the first frame is never retried.

        Raises ``RateLimitedError`` when the pool is full, ``UnavailableError`` when
        no sandbox could start, ``PoolDrainingError``, ``PoolNotLoadedError``,
        ``RequestTooLargeError`` for a body over the API's 1 MiB limit, and
        ``PoolRunError`` for a failure after the first frame and before ``exit``.
        """
        request = exec_request(command, args, cwd, env, stdin, timeout_seconds)
        body = json.dumps(request).encode("utf-8")
        if len(body) > _RUN_BODY_LIMIT:
            raise RequestTooLargeError(
                f"pool run: the encoded request is {len(body)} bytes, and the API takes {_RUN_BODY_LIMIT}. "
                "stdin is base64 inside it, so about 786 KiB of stdin fits."
            )
        try:
            response = await retrying(
                retry,
                lambda: self._transport.open_stream(
                    self._path("/run"), body=body, timeout=_run_timeout(timeout_seconds)
                ),
            )
        except ConflictError as error:
            raise await _named_conflict(self._transport, self.name, self._tenant_id, error, declaring=False) from error

        # Past the status line the command may have run, so nothing below retries.
        run: PoolRun | None = None
        saw_exit = False
        saw_done = False
        failure: RunStreamError | None = None
        try:
            async for line in response.aiter_lines():
                if not line.strip():
                    continue
                raw = json.loads(line)
                failure = decode_run_error(raw)
                if failure is not None:
                    break
                chunk = decode_run_chunk(raw)
                if chunk is None:
                    continue
                if chunk.kind == "run":
                    run = chunk.run
                elif chunk.kind == "exit":
                    saw_exit = True
                elif chunk.kind == "run_done":
                    saw_done = True
                yield chunk
        except (httpx.HTTPError, ValueError) as error:
            failure = RunStreamError("stream_ended", f"pool run: the stream broke: {error}")
        finally:
            await response.aclose()

        if saw_done and saw_exit:
            return
        if failure is None:
            failure = RunStreamError("stream_ended", "pool run: the stream ended early")
        if saw_exit:
            yield RunChunk("stream_error", error=failure)
            return
        raise PoolRunError(failure.code, failure.message, run=run)

    async def execs(self, sandbox_id: str) -> list[SandboxExec]:
        """The audit trail of one of this pool's sandboxes."""
        raw = await retrying(
            self._retry,
            lambda: self._transport.json(
                self._path(f"/sandboxes/{path_segment(sandbox_id)}/execs"), timeout=_CALL_TIMEOUT
            ),
        )
        return [decode_sandbox_exec(item) for item in (raw.get("execs") or [])]

    async def events(self, sandbox_id: str) -> list[SandboxEvent]:
        """The lifecycle and reset trail of one of this pool's sandboxes."""
        raw = await retrying(
            self._retry,
            lambda: self._transport.json(
                self._path(f"/sandboxes/{path_segment(sandbox_id)}/events"), timeout=_CALL_TIMEOUT
            ),
        )
        return [decode_sandbox_event(item) for item in (raw.get("events") or [])]

    async def delete(self, *, max_wait_seconds: float = 600.0) -> None:
        """Drain and delete the pool, returning once it is gone. Idempotent.

        Each call to the API waits up to about 25 s for running commands to finish,
        and the wait is repeated until the pool is gone or ``max_wait_seconds`` has
        passed, which raises ``PoolDrainingError``. A runner that is down is waited
        out the same way, and its last refusal is raised. Runs in progress finish.
        Calling ``delete`` again picks up the same drain.
        """
        sleep = self._retry.sleep if self._retry is not None else asyncio.sleep
        deadline = time.monotonic() + max_wait_seconds
        while True:
            try:
                response = await self._transport.request(self._path(), method="DELETE", timeout=_DELETE_TIMEOUT)
            except (RateLimitedError, UnavailableError, GatewayTimeoutError) as error:
                delay = error.retry_after if error.retry_after is not None else _DRAIN_POLL_SECONDS
                if time.monotonic() + delay > deadline:
                    raise
            except VoraError as error:
                if error.status == 404:
                    return
                raise
            else:
                if response.status_code != 202:
                    return
                delay = _DRAIN_POLL_SECONDS
                if time.monotonic() + delay > deadline:
                    raise PoolDrainingError(
                        f"pool {self.name} is still draining after {max_wait_seconds} s; call delete again",
                        status=202,
                    )
            await sleep(delay)
