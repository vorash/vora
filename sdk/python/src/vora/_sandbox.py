"""Sandbox handle: exec, files, stats and audit readers."""

from __future__ import annotations

from typing import Any, AsyncGenerator, AsyncIterator, Sequence
from urllib.parse import urlencode

from ._errors import VoraError
from ._http import Transport, path_segment
from ._types import (
    ExecChunk,
    ExecResult,
    SandboxEgress,
    SandboxEvent,
    SandboxExec,
    SandboxInfo,
    Stats,
    decode_exec_chunk,
    decode_sandbox_egress,
    decode_sandbox_event,
    decode_sandbox_exec,
    decode_sandbox_info,
    decode_stats,
    encode_exec_request,
)


def exec_request(
    command: str,
    args: Sequence[str] | None,
    cwd: str | None,
    env: Sequence[str] | None,
    stdin: str | bytes | None,
    timeout_seconds: int | None,
) -> dict[str, Any]:
    # A bare command is a shell command line; `/bin/sh` exists in every
    # Vora base image and keeps pipelines and quoting working the way
    # callers expect.
    if args is None:
        options: dict[str, Any] = {"command": "sh", "args": ["-c", command]}
    else:
        options = {"command": command, "args": list(args)}
    options.update(cwd=cwd, env=list(env) if env else None, stdin=stdin, timeout_seconds=timeout_seconds)
    return encode_exec_request(options)


class Sandbox:
    """A handle to one sandbox. Reached through ``vora.sandboxes``."""

    def __init__(self, transport: Transport, info: SandboxInfo) -> None:
        self._transport = transport
        self._info = info

    @property
    def id(self) -> str:
        return self._info.id

    @property
    def info(self) -> SandboxInfo:
        """The sandbox record as of the last call."""
        return self._info

    def _path(self, suffix: str = "") -> str:
        return f"/v1/sandboxes/{path_segment(self.id)}{suffix}"

    async def refresh(self) -> SandboxInfo:
        self._info = decode_sandbox_info(await self._transport.json(self._path()))
        return self._info

    async def exec(
        self,
        command: str,
        args: Sequence[str] | None = None,
        *,
        cwd: str | None = None,
        env: Sequence[str] | None = None,
        stdin: str | bytes | None = None,
        timeout_seconds: int | None = None,
    ) -> ExecResult:
        """Run a command and collect its output.

        With ``args=None`` the command is a shell line run via ``/bin/sh -c``;
        pass ``args`` (even empty) to run ``command`` literally.
        """
        stdout = bytearray()
        stderr = bytearray()
        exit_code = 0
        reason: str | None = None
        saw_exit = False
        request = exec_request(command, args, cwd, env, stdin, timeout_seconds)
        async for chunk in self._stream(request):
            if chunk.kind == "stdout":
                stdout += chunk.data
            elif chunk.kind == "stderr":
                stderr += chunk.data
            else:
                saw_exit = True
                exit_code = chunk.exit_code or 0
                reason = chunk.reason
        # A stream that ends without an exit record is a killed run, not a
        # successful one: fail loudly and hand back whatever output there was.
        if not saw_exit:
            raise VoraError(
                "exec: stream ended without an exit record (the run was killed)",
                body={"stdout": stdout.decode("utf-8", "replace"), "stderr": stderr.decode("utf-8", "replace")},
            )
        return ExecResult(
            stdout=stdout.decode("utf-8", "replace"),
            stderr=stderr.decode("utf-8", "replace"),
            exit_code=exit_code,
            reason=reason,
        )

    async def stream_exec(
        self,
        command: str,
        args: Sequence[str] | None = None,
        *,
        cwd: str | None = None,
        env: Sequence[str] | None = None,
        stdin: str | bytes | None = None,
        timeout_seconds: int | None = None,
    ) -> AsyncGenerator[ExecChunk, None]:
        """Stream exec output as chunks, ending with an ``exit`` chunk."""
        request = exec_request(command, args, cwd, env, stdin, timeout_seconds)
        async for chunk in self._stream(request):
            yield chunk

    async def _stream(self, request: dict[str, Any]) -> AsyncGenerator[ExecChunk, None]:
        async for raw in self._transport.ndjson(self._path("/exec"), json_body=request):
            chunk = decode_exec_chunk(raw)
            if chunk is not None:
                yield chunk

    async def write_file(self, path: str, content: str | bytes, *, create_parents: bool = True) -> int:
        query = urlencode({"path": path, "create_parents": "true" if create_parents else "false"})
        body = content.encode("utf-8") if isinstance(content, str) else content
        response = await self._transport.request(
            self._path("/files?") + query,
            method="PUT",
            body=body,
            content_type="application/octet-stream",
        )
        result = response.json()
        written = result.get("bytes_written") if isinstance(result, dict) else None
        return int(written) if isinstance(written, (int, str)) else len(body)

    async def read_file(self, path: str) -> bytes:
        return await self._transport.bytes(self._path("/files?") + urlencode({"path": path}))

    async def read_text(self, path: str) -> str:
        return (await self.read_file(path)).decode("utf-8")

    async def stats(self) -> Stats:
        return decode_stats(await self._transport.json(self._path("/stats")))

    async def events(self) -> list[SandboxEvent]:
        raw = await self._transport.json(self._path("/events"))
        return [decode_sandbox_event(item) for item in (raw.get("events") or [])]

    async def execs(self) -> list[SandboxExec]:
        raw = await self._transport.json(self._path("/execs"))
        return [decode_sandbox_exec(item) for item in (raw.get("execs") or [])]

    async def egress(self) -> list[SandboxEgress]:
        raw = await self._transport.json(self._path("/egress"))
        return [decode_sandbox_egress(item) for item in (raw.get("records") or [])]

    async def destroy(self) -> None:
        await self._transport.request(self._path(), method="DELETE")
