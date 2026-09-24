"""Snapshot capture and forks (ADR 023)."""

from __future__ import annotations

from ._http import Transport, path_segment
from ._sandbox import Sandbox
from ._types import SnapshotInfo, decode_sandbox_info, decode_snapshot_info


class Snapshots:
    """Snapshot operations, reached through ``vora.snapshots``."""

    def __init__(self, transport: Transport) -> None:
        self._transport = transport

    async def create(self, sandbox_id: str) -> "Snapshot":
        raw = await self._transport.json(f"/v1/sandboxes/{path_segment(sandbox_id)}/snapshot", method="POST")
        return Snapshot(self._transport, decode_snapshot_info(raw))

    async def get(self, id: str) -> "Snapshot":
        raw = await self._transport.json(f"/v1/snapshots/{path_segment(id)}")
        return Snapshot(self._transport, decode_snapshot_info(raw))

    async def list(self) -> list["Snapshot"]:
        raw = await self._transport.json("/v1/snapshots")
        return [Snapshot(self._transport, decode_snapshot_info(item)) for item in (raw.get("snapshots") or [])]

    async def remove(self, id: str) -> None:
        await self._transport.request(f"/v1/snapshots/{path_segment(id)}", method="DELETE")

    async def fork(self, id: str) -> Sandbox:
        raw = await self._transport.json(f"/v1/snapshots/{path_segment(id)}/fork", method="POST")
        return Sandbox(self._transport, decode_sandbox_info(raw))


class Snapshot:
    """A handle to one snapshot."""

    def __init__(self, transport: Transport, info: SnapshotInfo) -> None:
        self._transport = transport
        self._info = info

    @property
    def id(self) -> str:
        return self._info.id

    @property
    def info(self) -> SnapshotInfo:
        return self._info

    async def refresh(self) -> SnapshotInfo:
        self._info = decode_snapshot_info(await self._transport.json(f"/v1/snapshots/{path_segment(self.id)}"))
        return self._info

    async def fork(self) -> Sandbox:
        raw = await self._transport.json(f"/v1/snapshots/{path_segment(self.id)}/fork", method="POST")
        return Sandbox(self._transport, decode_sandbox_info(raw))

    async def remove(self) -> None:
        await self._transport.request(f"/v1/snapshots/{path_segment(self.id)}", method="DELETE")
