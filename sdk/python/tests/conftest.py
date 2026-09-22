"""Shared test fixture: a vora-api stand-in that records every request."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any, Callable, Iterator

import httpx
import pytest

from vora import Vora

Json = dict[str, Any]
Handler = Callable[["RecordedRequest"], "FakeResponse | None"]


@dataclass
class RecordedRequest:
    method: str
    url: str
    headers: dict[str, str]
    body: bytes

    @property
    def json(self) -> Json:
        return json.loads(self.body) if self.body else {}


@dataclass
class FakeResponse:
    status: int = 200
    json_body: Any = None
    text: str | None = None
    bytes_body: bytes | None = None
    ndjson: list[Any] | None = None


@dataclass
class FakeAPI:
    requests: list[RecordedRequest] = field(default_factory=list)
    http: Any = None

    @property
    def last(self) -> RecordedRequest:
        return self.requests[-1]


def default_response(request: RecordedRequest) -> FakeResponse:
    if request.method == "POST" and request.url.endswith("/v1/sandboxes"):
        return FakeResponse(status=201, json_body={"id": "sbx_test", "state": "RUNNING"})
    return FakeResponse(json_body={"id": "sbx_test", "state": "RUNNING"})


def make_client(handler: Handler | None = None) -> tuple[Vora, FakeAPI]:
    api = FakeAPI()

    def route(request: httpx.Request) -> httpx.Response:
        recorded = RecordedRequest(
            method=request.method,
            url=str(request.url),
            headers={key.lower(): value for key, value in request.headers.items()},
            body=request.content,
        )
        api.requests.append(recorded)
        fake = (handler(recorded) if handler else None) or default_response(recorded)
        if fake.ndjson is not None:
            body = "".join(json.dumps(item) + "\n" for item in fake.ndjson)
            return httpx.Response(fake.status, headers={"content-type": "application/x-ndjson"}, text=body)
        if fake.bytes_body is not None:
            return httpx.Response(fake.status, content=fake.bytes_body)
        if fake.text is not None:
            return httpx.Response(fake.status, text=fake.text)
        return httpx.Response(fake.status, json=fake.json_body)

    http = httpx.AsyncClient(transport=httpx.MockTransport(route))
    api.http = http
    return Vora(base_url="http://vora.test", api_key="vora_test", client=http), api


@pytest.fixture
def boot() -> Iterator[Callable[[Handler | None], tuple[Vora, FakeAPI]]]:
    def start(handler: Handler | None = None) -> tuple[Vora, FakeAPI]:
        return make_client(handler)

    yield start
