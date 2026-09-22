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
