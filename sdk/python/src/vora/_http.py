"""Thin HTTP layer: base URL, bearer auth, and error normalisation."""

from __future__ import annotations

import asyncio
import json
import random
from dataclasses import dataclass
from datetime import datetime, timezone
from email.utils import parsedate_to_datetime
from typing import Any, AsyncIterator, Awaitable, Callable, TypeVar
from urllib.parse import quote

import httpx

from ._errors import (
    ConflictError,
    GatewayTimeoutError,
    RateLimitedError,
    RetryLaterError,
    UnavailableError,
    VoraError,
)

T = TypeVar("T")


def path_segment(id: str) -> str:
    if not id or any(part in (".", "..") for part in id.split("/")):
        raise ValueError("invalid resource id")
    return quote(id, safe="")


@dataclass(frozen=True)
class RetryPolicy:
    """How to repeat a call the API refused with 429 or 503.

    A ``Retry-After`` longer than ``max_delay`` raises instead of waiting, so the
    whole wait stays under ``max_attempts * max_delay``.
    """

    max_attempts: int = 4
    """Attempts in total, the first included."""
    base_delay: float = 0.25
    """Seconds before the first retry when the API names no wait; doubles each time."""
    max_delay: float = 10.0
    sleep: Callable[[float], Awaitable[None]] = asyncio.sleep


def _retry_delay(policy: RetryPolicy, attempt: int, error: RetryLaterError) -> float | None:
    if error.retry_after is not None:
        if error.retry_after > policy.max_delay:
            return None
        # Added, never subtracted: many callers refused together must not all return
        # at the same instant, and none may come back before the API asked.
        return error.retry_after + random.uniform(0, policy.base_delay)
    return min(policy.base_delay * 2.0 ** (attempt - 1), policy.max_delay)


async def retrying(policy: RetryPolicy | None, call: Callable[[], Awaitable[T]]) -> T:
    """Run ``call``, repeating it on 429 and 503 as ``policy`` allows."""
    attempt = 1
    while True:
        try:
            return await call()
        except (RateLimitedError, UnavailableError) as error:
            if policy is None or attempt >= policy.max_attempts:
                raise
            delay = _retry_delay(policy, attempt, error)
            if delay is None:
                raise
            await policy.sleep(delay)
            attempt += 1


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
        timeout: httpx.Timeout | None = None,
    ) -> httpx.Response:
        """Perform a request, raising ``VoraError`` on any non-2xx response."""
        try:
            response = await self._client.request(
                method,
                self.base_url + path,
                headers=self._headers(content_type),
                content=body,
                timeout=httpx.USE_CLIENT_DEFAULT if timeout is None else timeout,
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
        timeout: httpx.Timeout | None = None,
    ) -> Any:
        body = None if json_body is None else json.dumps(json_body).encode("utf-8")
        response = await self.request(
            path,
            method=method,
            body=body,
            content_type="application/json" if body is not None else None,
            timeout=timeout,
        )
        return response.json()

    async def open_stream(self, path: str, *, body: bytes, timeout: httpx.Timeout) -> httpx.Response:
        """POST and return the response once its status is known to be good.

        The caller reads and closes it. Splitting this from reading the frames is what
        lets a retry wrap the refusal and nothing after it.
        """
        request = self._client.build_request(
            "POST",
            self.base_url + path,
            headers=self._headers("application/json"),
            content=body,
            timeout=timeout,
        )
        try:
            response = await self._client.send(request, stream=True)
        except httpx.HTTPError as error:
            raise VoraError(str(error)) from error
        if response.is_error:
            try:
                await response.aread()
            except httpx.HTTPError:
                pass
            finally:
                await response.aclose()
            raise _to_vora_error(response)
        return response

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
    status = response.status_code
    retry_after = _parse_retry_after(response.headers.get("retry-after"))
    if status == 429:
        return RateLimitedError(message, status=status, body=body, retry_after=retry_after)
    if status == 503:
        return UnavailableError(message, status=status, body=body, retry_after=retry_after)
    if status == 504:
        return GatewayTimeoutError(message, status=status, body=body, retry_after=retry_after)
    if status == 409:
        return ConflictError(message, status=status, body=body)
    return VoraError(message, status=status, body=body)


def _parse_retry_after(value: str | None) -> float | None:
    """Seconds, or an HTTP date from a proxy in front of the API."""
    if value is None:
        return None
    try:
        return max(float(value), 0.0)
    except ValueError:
        pass
    try:
        when = parsedate_to_datetime(value)
    except (TypeError, ValueError):
        return None
    if when.tzinfo is None:
        when = when.replace(tzinfo=timezone.utc)
    return max((when - datetime.now(timezone.utc)).total_seconds(), 0.0)
