"""The sync facade drives the async client without an event loop in the caller."""

from __future__ import annotations

import asyncio
import base64
import contextlib
import threading
import time
from typing import Any, Callable, Iterator

import httpx
import pytest

from vora import Pool as AsyncPool
from vora import RateLimitedError, RetryPolicy
from vora import Sandbox as AsyncSandbox
from vora._types import SandboxInfo, decode_pool_info
from vora.sync import Pool, Sandbox, Vora

from conftest import FakeResponse, default_response, make_client
from wire_server import Reply, Sent, WireServer, run_reply


def test_sync_create_exec_destroy() -> None:
    def handler(request):
        if request.url.endswith("/exec"):
            return FakeResponse(ndjson=[
                {"stdout": base64.b64encode(b"hi\n").decode()},
                {"exit": {"exitCode": 0, "reason": "EXITED"}},
            ])
        return default_response(request)

    _, api = make_client(handler)
    client = Vora(base_url="http://vora.test", api_key="vora_test", client=api.http)
    try:
        sandbox = client.sandboxes.create(image="vora/python")
        assert sandbox.id == "sbx_test"

        result = sandbox.exec("echo hi")
        assert result.stdout == "hi\n"
        assert result.exit_code == 0

        chunks = list(sandbox.stream_exec("echo hi"))
        assert chunks[-1].exit_code == 0

        sandbox.destroy()
        assert api.last.method == "DELETE"
    finally:
        client.close()


def test_sync_pool_runs_streams_and_deletes() -> None:
    frames = [
        {"run": {"sandboxId": "sbx_1", "warm": True, "sandboxRun": 2, "reuse": "POOL_REUSE_RESET"}},
        {"stdout": base64.b64encode(b"hi\n").decode()},
        {"exit": {"reason": "EXITED"}},
        {"runDone": {"outcome": "POOL_RESET_OK"}},
    ]

    def handler(request):
        if request.url.endswith("/v1/pools/py/run"):
            return FakeResponse(ndjson=frames)
        if request.method == "DELETE":
            return FakeResponse(status=204, text="")
        return FakeResponse(json_body={"id": "pool_1", "spec": {"name": "py"}, "state": "POOL_ACTIVE", "stats": {"idle": 1}})

    _, api = make_client(handler)
    client = Vora(base_url="http://vora.test", api_key="vora_test", client=api.http)
    try:
        pool = client.pools.declare("py", image="vora/base", cpu="100m", memory="1Mi", size=1)
        assert pool.id == "pool_1"
        assert client.pools.get("py").name == "py"

        result = pool.run("true", [], timeout_seconds=5)
        assert result.stdout == "hi\n"
        assert result.run.sandbox_run == 2
        assert result.done is not None and result.done.outcome == "POOL_RESET_OK"

        kinds = [chunk.kind for chunk in pool.stream_run("true", [], timeout_seconds=5)]
        assert kinds == ["run", "stdout", "exit", "run_done"]

        stats = pool.stats()
        assert stats is not None and stats.idle == 1
        pool.delete()
    finally:
        client.close()


# --- The client's retry policy reaches the async client ---------------------------


def declare_refused() -> tuple[Any, Any]:
    _, api = make_client(lambda _: FakeResponse(status=429, json_body={"error": "full"}))
    return api, {"image": "vora/base", "cpu": "100m", "memory": "1Mi"}


def test_sync_retry_none_turns_retries_off() -> None:
    api, spec = declare_refused()
    client = Vora(base_url="http://vora.test", client=api.http, retry=None)
    try:
        with pytest.raises(RateLimitedError):
            client.pools.declare("py", **spec)
    finally:
        client.close()

    assert len(api.requests) == 1


def test_sync_retry_policy_sets_the_attempts() -> None:
    api, spec = declare_refused()
    waited: list[float] = []

    async def sleep(seconds: float) -> None:
        waited.append(seconds)

    client = Vora(base_url="http://vora.test", client=api.http, retry=RetryPolicy(max_attempts=2, sleep=sleep))
    try:
        with pytest.raises(RateLimitedError):
            client.pools.declare("py", **spec)
    finally:
        client.close()

    assert len(api.requests) == 2
    assert len(waited) == 1


# --- Over a real socket, which a private event loop must not share ------------------


@pytest.fixture
def socket_server() -> Iterator[Callable[[Callable[[Sent], Reply]], WireServer]]:
    loop = asyncio.new_event_loop()
    thread = threading.Thread(target=loop.run_forever, daemon=True)
    thread.start()
    servers: list[WireServer] = []

    def start(route: Callable[[Sent], Reply]) -> WireServer:
        server = WireServer(route)
        asyncio.run_coroutine_threadsafe(server.start(), loop).result(timeout=5)
        servers.append(server)
        return server

    yield start
    for server in servers:
        asyncio.run_coroutine_threadsafe(server.stop(), loop).result(timeout=5)
    loop.call_soon_threadsafe(loop.stop)
    thread.join(timeout=5)
    loop.close()


def wait_for_client_to_leave(server: WireServer) -> bool:
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        if server.client_left.is_set():
            return True
        time.sleep(0.01)
    return False


def pool_on(client: Vora) -> Pool:
    info = decode_pool_info({"id": "pool_test", "spec": {"name": "py"}})
    return Pool(client._loop, AsyncPool(client._async._transport, None, "py", info, None))


def test_a_held_sync_stream_left_early_disconnects(socket_server) -> None:
    server = socket_server(lambda _: run_reply("warm_reset", cut_after=1, hold=True))
    client = Vora(base_url=server.url, api_key="vora_test")
    try:
        stream = pool_on(client).stream_run("sleep 60", timeout_seconds=60)
        for chunk in stream:
            assert chunk.kind == "run"
            break

        assert wait_for_client_to_leave(server)
        assert stream is not None
    finally:
        client.close()


def test_closing_a_sync_iterator_disconnects(socket_server) -> None:
    server = socket_server(lambda _: run_reply("warm_reset", cut_after=1, hold=True))
    client = Vora(base_url=server.url, api_key="vora_test")
    try:
        chunks = iter(pool_on(client).stream_run("sleep 60", timeout_seconds=60))
        with contextlib.closing(chunks):
            assert next(chunks).kind == "run"
            assert not server.client_left.is_set()

        assert wait_for_client_to_leave(server)
    finally:
        client.close()


EXITED = '{"exit":{"exitCode":0,"reason":"EXITED"}}'


def _exec(sandbox: Sandbox, **options: Any) -> Any:
    return sandbox.exec("sleep 7", **options)


def _stream_exec(sandbox: Sandbox, **options: Any) -> Any:
    return list(sandbox.stream_exec("sleep 7", **options))


both_exec_calls = pytest.mark.parametrize("call", [_exec, _stream_exec], ids=["exec", "stream_exec"])


def sandbox_on(client: Vora) -> Sandbox:
    return Sandbox(client._loop, AsyncSandbox(client._async._transport, SandboxInfo(id="sbx_1")))


@contextlib.contextmanager
def client_with_read_timeout(server: WireServer, read_timeout: float) -> Iterator[Vora]:
    http = httpx.AsyncClient(timeout=read_timeout)
    client = Vora(base_url=server.url, client=http)
    try:
        yield client
    finally:
        client._run(http.aclose())
        client.close()


@both_exec_calls
def test_sync_exec_keeps_a_callers_read_timeout_with_none_of_its_own(socket_server, call) -> None:
    server = socket_server(lambda _: Reply(lines=[EXITED], delay=0.3))
    with client_with_read_timeout(server, 0.05) as client, pytest.raises(httpx.ReadTimeout):
        call(sandbox_on(client))


@both_exec_calls
def test_sync_exec_timeout_outlasts_a_callers_read_timeout(socket_server, call) -> None:
    server = socket_server(lambda _: Reply(lines=[EXITED], delay=0.3))
    with client_with_read_timeout(server, 0.05) as client:
        call(sandbox_on(client), timeout_seconds=10)


@both_exec_calls
def test_sync_exec_raises_on_silence_past_the_margin(socket_server, call, monkeypatch) -> None:
    monkeypatch.setattr("vora._http._EXEC_READ_MARGIN_SECONDS", 0.2)
    server = socket_server(lambda _: Reply(lines=[], hold=True))
    raised: list[httpx.ReadTimeout] = []

    def read(client: Vora) -> None:
        try:
            call(sandbox_on(client), timeout_seconds=1)
        except httpx.ReadTimeout as error:
            raised.append(error)

    with client_with_read_timeout(server, 60.0) as client:
        # In a thread, so a read with no bound fails this test instead of hanging it.
        thread = threading.Thread(target=read, args=(client,), daemon=True)
        thread.start()
        thread.join(timeout=10)
        assert raised


@both_exec_calls
def test_sync_client_the_sdk_built_waits_with_no_exec_timeout(socket_server, call) -> None:
    server = socket_server(lambda _: Reply(lines=[EXITED], delay=0.3))
    client = Vora(base_url=server.url)
    # A client the SDK owns, with a default shorter than the silence to keep this short.
    client._run(client._async._transport._client.aclose())
    client._async._transport._client = httpx.AsyncClient(timeout=0.05)
    try:
        call(sandbox_on(client))
    finally:
        client.close()
