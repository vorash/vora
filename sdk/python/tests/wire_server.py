"""A real socket that answers with bytes the real API sent, as ``sdk/testdata`` holds them."""

from __future__ import annotations

import asyncio
import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

GOLDEN: dict[str, Any] = json.loads((Path(__file__).parents[2] / "testdata" / "pool_wire.json").read_text())


@dataclass
class Sent:
    method: str
    path: str
    headers: dict[str, str]
    body: bytes


@dataclass
class Reply:
    status: int = 200
    retry_after: str | None = None
    body: str = ""
    lines: list[str] | None = None
    # Split the stream's bytes into pieces this long, so a frame crosses reads.
    piece: int = 7
    # Cut the connection after this many lines, with no terminating chunk.
    cut_after: int | None = None
    # Answer the status and these lines, then wait for the client to leave.
    hold: bool = False
    # Say nothing for this long first, as a silent command or a slow first frame does.
    delay: float = 0.0


def run_reply(name: str, **overrides: Any) -> Reply:
    scenario = GOLDEN["runs"][name]
    return Reply(
        status=scenario["status"],
        retry_after=scenario.get("retryAfter"),
        body=scenario.get("body", ""),
        lines=scenario.get("lines"),
        **overrides,
    )


def answer_reply(name: str) -> Reply:
    scenario = GOLDEN["answers"][name]
    return Reply(status=scenario["status"], retry_after=scenario.get("retryAfter"), body=scenario.get("body", ""))


@dataclass
class WireServer:
    route: Callable[[Sent], Reply]
    requests: list[Sent] = field(default_factory=list)
    client_left: asyncio.Event = field(default_factory=asyncio.Event)
    url: str = ""
    _server: Any = None

    async def start(self) -> None:
        self._server = await asyncio.start_server(self._serve, "127.0.0.1", 0)
        port = self._server.sockets[0].getsockname()[1]
        self.url = f"http://127.0.0.1:{port}"

    async def stop(self) -> None:
        self._server.close()
        await self._server.wait_closed()

    async def _serve(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        try:
            while await self._one(reader, writer):
                pass
        except (ConnectionError, asyncio.IncompleteReadError):
            pass
        finally:
            writer.close()

    async def _one(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> bool:
        head = await reader.readuntil(b"\r\n\r\n")
        request_line, *header_lines = head.decode().split("\r\n")
        method, path, _ = request_line.split(" ")
        headers = {}
        for line in header_lines:
            if line:
                key, value = line.split(":", 1)
                headers[key.strip().lower()] = value.strip()
        body = await reader.readexactly(int(headers.get("content-length", "0")))
        sent = Sent(method, path, headers, body)
        self.requests.append(sent)
        reply = self.route(sent)
        await asyncio.sleep(reply.delay)

        phrase = {200: "OK", 201: "Created", 202: "Accepted", 204: "No Content"}.get(reply.status, "Refused")
        out = [f"HTTP/1.1 {reply.status} {phrase}"]
        if reply.retry_after is not None:
            out.append(f"Retry-After: {reply.retry_after}")
        if reply.lines is None:
            data = reply.body.encode()
            if reply.status != 204:
                out.append(f"Content-Length: {len(data)}")
            out.append("Content-Type: application/json")
            writer.write(("\r\n".join(out) + "\r\n\r\n").encode() + data)
            await writer.drain()
            return True

        out += ["Content-Type: application/x-ndjson", "Transfer-Encoding: chunked"]
        writer.write(("\r\n".join(out) + "\r\n\r\n").encode())
        lines = reply.lines if reply.cut_after is None else reply.lines[: reply.cut_after]
        stream = "".join(line + "\n" for line in lines).encode()
        for start in range(0, len(stream), reply.piece):
            piece = stream[start : start + reply.piece]
            writer.write(f"{len(piece):x}\r\n".encode() + piece + b"\r\n")
            await writer.drain()
        if reply.hold:
            await reader.read()
            self.client_left.set()
            return False
        if reply.cut_after is not None:
            return False
        writer.write(b"0\r\n\r\n")
        await writer.drain()
        return True
