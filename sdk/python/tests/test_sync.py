"""The sync facade drives the async client without an event loop in the caller."""

from __future__ import annotations

import base64

from vora.sync import Vora

from conftest import FakeResponse, default_response, make_client


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
