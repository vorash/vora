"""Synchronous facade over the asyncio client.

A single-threaded convenience for scripts and notebooks: each call drives a
private event loop. It is not thread-safe — use the asyncio client
(``vora.Vora``) for concurrent work.
"""

from __future__ import annotations

import asyncio
from typing import Any, AsyncIterator, Awaitable, Iterator, TypeVar

from ._client import Vora as AsyncVora
from ._client import Sandboxes as AsyncSandboxes
from ._sandbox import Sandbox as AsyncSandbox
from ._snapshots import Snapshot as AsyncSnapshot
from ._snapshots import Snapshots as AsyncSnapshots
from ._types import (
    ExecChunk,
    ExecResult,
    SandboxEgress,
    SandboxEvent,
    SandboxExec,
    SandboxInfo,
    SnapshotInfo,
    Stats,
)


T = TypeVar("T")


class _Sync:
    def __init__(self, loop: asyncio.AbstractEventLoop) -> None:
        self._loop = loop

    def _run(self, coro: Awaitable[T]) -> T:
        return self._loop.run_until_complete(coro)


class Vora(_Sync):
    """Entry point: ``from vora.sync import Vora``."""

    def __init__(
        self,
        *,
        base_url: str | None = None,
        api_key: str | None = None,
        client: Any = None,
    ) -> None:
        loop = asyncio.new_event_loop()
        super().__init__(loop)
        self._async = AsyncVora(base_url=base_url, api_key=api_key, client=client)
        self.sandboxes = Sandboxes(loop, self._async.sandboxes)
        self.snapshots = Snapshots(loop, self._async.snapshots)

    def close(self) -> None:
        self._run(self._async.aclose())
        self._loop.close()

    def __enter__(self) -> "Vora":
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()


class Sandboxes(_Sync):
    def __init__(self, loop: asyncio.AbstractEventLoop, async_: AsyncSandboxes) -> None:
        super().__init__(loop)
        self._async = async_

    def create(self, **options: Any) -> "Sandbox":
        return Sandbox(self._loop, self._run(self._async.create(**options)))

    def get(self, id: str) -> "Sandbox":
        return Sandbox(self._loop, self._run(self._async.get(id)))

    def list(self, *, include_destroyed: bool = False) -> list["Sandbox"]:
        return [Sandbox(self._loop, item) for item in self._run(self._async.list(include_destroyed=include_destroyed))]

    def destroy(self, id: str) -> None:
        self._run(self._async.destroy(id))


class Sandbox(_Sync):
    def __init__(self, loop: asyncio.AbstractEventLoop, async_: AsyncSandbox) -> None:
        super().__init__(loop)
        self._async = async_

    @property
    def id(self) -> str:
        return self._async.id

    @property
    def info(self) -> SandboxInfo:
        return self._async.info

    def refresh(self) -> SandboxInfo:
        return self._run(self._async.refresh())

    def exec(self, command: str, args: list[str] | None = None, **options: Any) -> ExecResult:
        return self._run(self._async.exec(command, args, **options))

    def stream_exec(self, command: str, args: list[str] | None = None, **options: Any) -> Iterator[ExecChunk]:
        agen = self._async.stream_exec(command, args, **options)
        try:
            while True:
                try:
                    yield self._run(agen.__anext__())
                except StopAsyncIteration:
                    return
        finally:
            self._run(agen.aclose())

    def write_file(self, path: str, content: str | bytes, *, create_parents: bool = True) -> int:
        return self._run(self._async.write_file(path, content, create_parents=create_parents))

    def read_file(self, path: str) -> bytes:
        return self._run(self._async.read_file(path))

    def read_text(self, path: str) -> str:
        return self._run(self._async.read_text(path))

    def stats(self) -> Stats:
        return self._run(self._async.stats())

    def events(self) -> list[SandboxEvent]:
        return self._run(self._async.events())

    def execs(self) -> list[SandboxExec]:
        return self._run(self._async.execs())

    def egress(self) -> list[SandboxEgress]:
        return self._run(self._async.egress())

    def destroy(self) -> None:
        self._run(self._async.destroy())


class Snapshots(_Sync):
    def __init__(self, loop: asyncio.AbstractEventLoop, async_: AsyncSnapshots) -> None:
        super().__init__(loop)
        self._async = async_

    def create(self, sandbox_id: str) -> "Snapshot":
        return Snapshot(self._loop, self._run(self._async.create(sandbox_id)))

    def get(self, id: str) -> "Snapshot":
        return Snapshot(self._loop, self._run(self._async.get(id)))

    def list(self) -> list["Snapshot"]:
        return [Snapshot(self._loop, item) for item in self._run(self._async.list())]

    def remove(self, id: str) -> None:
        self._run(self._async.remove(id))

    def fork(self, id: str) -> Sandbox:
        return Sandbox(self._loop, self._run(self._async.fork(id)))


class Snapshot(_Sync):
    def __init__(self, loop: asyncio.AbstractEventLoop, async_: AsyncSnapshot) -> None:
        super().__init__(loop)
        self._async = async_

    @property
    def id(self) -> str:
        return self._async.id

    @property
    def info(self) -> SnapshotInfo:
        return self._async.info

    def refresh(self) -> SnapshotInfo:
        return self._run(self._async.refresh())

    def fork(self) -> Sandbox:
        return Sandbox(self._loop, self._run(self._async.fork()))

    def remove(self) -> None:
        self._run(self._async.remove())
