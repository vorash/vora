"""Sandbox lifecycle operations, reached through ``vora.sandboxes``."""

from __future__ import annotations

import os
from typing import Any

import httpx

from ._http import RetryPolicy, Transport, path_segment
from ._pools import Pools
from ._sandbox import Sandbox
from ._snapshots import Snapshots
from ._types import decode_sandbox_info, encode_sandbox_spec

_DEFAULT_BASE_URL = "http://127.0.0.1:9300"


def _env(name: str) -> str | None:
    value = os.environ.get(name)
    return value or None


class Vora:
    """Entry point: ``client = vora.Vora()``."""

    def __init__(
        self,
        *,
        base_url: str | None = None,
        api_key: str | None = None,
        client: httpx.AsyncClient | None = None,
        retry: RetryPolicy | None = RetryPolicy(),
    ) -> None:
        """
        Args:
            base_url: Base URL of vora-api. Defaults to ``VORA_API_URL``, then
                ``http://127.0.0.1:9300``.
            api_key: Bearer API key. Defaults to ``VORA_API_KEY``.
            client: An ``httpx.AsyncClient`` to reuse (custom transports, tests).
            retry: How pool calls that are safe to repeat handle 429 and 503. None
                turns it off. A pool run has its own ``retry`` argument, off by
                default, ``delete`` waits by its own ``max_wait_seconds`` whatever this
                is, and sandbox routes do not retry.
        """
        self._transport = Transport(
            (base_url or _env("VORA_API_URL") or _DEFAULT_BASE_URL),
            api_key if api_key is not None else _env("VORA_API_KEY"),
            client=client,
        )
        self.sandboxes = Sandboxes(self._transport)
        self.snapshots = Snapshots(self._transport)
        self.pools = Pools(self._transport, retry)

    async def aclose(self) -> None:
        await self._transport.aclose()

    async def __aenter__(self) -> "Vora":
        return self

    async def __aexit__(self, *exc_info: object) -> None:
        await self.aclose()


class Sandboxes:
    """Sandbox lifecycle operations."""

    def __init__(self, transport: Transport) -> None:
        self._transport = transport

    async def create(self, **options: Any) -> Sandbox:
        """Create and start a sandbox, returning a handle to it.

        Keyword options mirror the TS SDK: ``image`` (required), ``backend``,
        ``agent_id``, ``tenant_id``, ``memory``, ``cpu``, ``pids``, ``disk``,
        ``network``, ``env``, ``ttl``.
        """
        raw = await self._transport.json("/v1/sandboxes", method="POST", json_body=encode_sandbox_spec(options))
        return Sandbox(self._transport, decode_sandbox_info(raw))

    async def get(self, id: str) -> Sandbox:
        raw = await self._transport.json(f"/v1/sandboxes/{path_segment(id)}")
        return Sandbox(self._transport, decode_sandbox_info(raw))

    async def list(self, *, include_destroyed: bool = False) -> list[Sandbox]:
        query = "?include_destroyed=true" if include_destroyed else ""
        raw = await self._transport.json(f"/v1/sandboxes{query}")
        return [Sandbox(self._transport, decode_sandbox_info(item)) for item in (raw.get("sandboxes") or [])]

    async def destroy(self, id: str) -> None:
        """Destroy a sandbox by id, without constructing a handle first."""
        await self._transport.request(f"/v1/sandboxes/{path_segment(id)}", method="DELETE")
