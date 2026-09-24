"""Thin HTTP layer: base URL, bearer auth, and error normalisation."""

from __future__ import annotations

import json
from typing import Any, AsyncIterator
from urllib.parse import quote

import httpx

from ._errors import VoraError


def path_segment(id: str) -> str:
    if not id or any(part in (".", "..") for part in id.split("/")):
        raise ValueError("invalid resource id")
    return quote(id, safe="")


class Transport:
    """Thin HTTP layer: base URL, bearer auth, and error normalisation."""

    def __init__(
        self,
        base_url: str,
        api_key: str | None = None,
        client: httpx.AsyncClient | None = None,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self._api_key = api_key
        self._client = client or httpx.AsyncClient()
        self._owns_client = client is None

    async def aclose(self) -> None:
        if self._owns_client:
            await self._client.aclose()

    def _headers(self, content_type: str | None) -> dict[str, str]:
        headers: dict[str, str] = {}
        if self._api_key:
            headers["Authorization"] = f"Bearer {self._api_key}"
        if content_type is not None:
            headers["Content-Type"] = content_type
        return headers

    async def request(
        self,
        path: str,
        *,
        method: str = "GET",
        body: bytes | None = None,
        content_type: str | None = None,
    ) -> httpx.Response:
        """Perform a request, raising ``VoraError`` on any non-2xx response."""
        try:
            response = await self._client.request(
                method,
                self.base_url + path,
                headers=self._headers(content_type),
                content=body,
            )
        except httpx.HTTPError as error:
            raise VoraError(str(error)) from error
        if response.is_error:
            raise _to_vora_error(response)
        return response

    async def json(
        self,
        path: str,
        *,
        method: str = "GET",
        json_body: Any = None,
    ) -> Any:
        body = None if json_body is None else json.dumps(json_body).encode("utf-8")
        response = await self.request(
            path,
            method=method,
            body=body,
            content_type="application/json" if body is not None else None,
        )
        return response.json()

    async def bytes(self, path: str) -> bytes:
        response = await self.request(path)
        return response.content

    async def ndjson(self, path: str, *, json_body: Any) -> AsyncIterator[Any]:
        """Stream one parsed JSON value per newline-delimited line."""
        body = json.dumps(json_body).encode("utf-8")
        async with self._client.stream(
            "POST",
            self.base_url + path,
            headers=self._headers("application/json"),
            content=body,
        ) as response:
            if response.is_error:
                await response.aread()
                raise _to_vora_error(response)
            pending = ""
            async for raw in response.aiter_text():
                pending += raw
                while "\n" in pending:
                    line, pending = pending.split("\n", 1)
                    if line.strip():
                        yield json.loads(line)
            if pending.strip():
                yield json.loads(pending)


def _to_vora_error(response: httpx.Response) -> VoraError:
    try:
        text = response.text
    except Exception:  # noqa: BLE001 - a broken body must not mask the failure
        text = ""
    message = f"{response.status_code} {response.reason_phrase}".strip()
    body: Any = text or None
    if text:
        try:
            parsed = json.loads(text)
            body = parsed
            detail = parsed.get("error") if isinstance(parsed, dict) else None
            if isinstance(detail, str) and detail:
                message = detail
        except ValueError:
            message = text[:300]
    return VoraError(message, status=response.status_code, body=body)
