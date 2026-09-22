"""Client-level behaviour: spec encoding parity with the TS SDK and errors."""

from __future__ import annotations

import pytest

from vora import VoraError

from conftest import FakeResponse, Handler


async def test_create_encodes_the_ts_wire_body(boot) -> None:
    client, api = boot()
    await client.sandboxes.create(
        image="vora/python",
        backend="bwrap",
        agent_id="analyst",
        tenant_id="tenant-a",
        memory="512Mi",
        cpu=1,
        pids=64,
        disk="2Gi",
        network={"mode": "restricted", "allowed_domains": ["pypi.org"], "broker_aliases": ["github"]},
        env={"LANG": "C"},
        ttl="1h30m",
    )
    assert api.last.method == "POST"
    assert api.last.url.endswith("/v1/sandboxes")
    assert api.last.headers["authorization"] == "Bearer vora_test"
    assert api.last.json == {
        "image": "vora/python",
        "backend": "bwrap",
        "agentId": "analyst",
        "tenantId": "tenant-a",
        "resources": {
            "cpuMillis": "1000",
            "memoryBytes": str(512 * 1024**2),
            "diskBytes": str(2 * 1024**3),
            "pids": "64",
        },
        "network": {
            "mode": "NETWORK_RESTRICTED",
            "allowedDomains": ["pypi.org"],
            "brokerAliases": ["github"],
        },
        "env": ["LANG=C"],
        "ttlSeconds": "5400",
    }


async def test_create_requires_an_image(boot) -> None:
    client, _ = boot()
    with pytest.raises(VoraError, match="image is required"):
        await client.sandboxes.create(memory="512Mi")


async def test_get_list_and_destroy(boot) -> None:
    client, api = boot(lambda r: FakeResponse(json_body={"sandboxes": [{"id": "sbx_1"}, {"id": "sbx_2"}]}))
    sandboxes = await client.sandboxes.list(include_destroyed=True)
    assert [s.id for s in sandboxes] == ["sbx_1", "sbx_2"]
    assert api.last.url.endswith("/v1/sandboxes?include_destroyed=true")

    await client.sandboxes.get("sbx_1")
    assert api.last.url.endswith("/v1/sandboxes/sbx_1")

    await client.sandboxes.destroy("sbx_1")
    assert api.last.method == "DELETE"


async def test_non_2xx_raises_vora_error_with_status_and_body(boot) -> None:
    handler: Handler = lambda r: FakeResponse(status=404, json_body={"error": "sandbox not found"})
    client, _ = boot(handler)
    with pytest.raises(VoraError) as excinfo:
        await client.sandboxes.get("sbx_nope")
    assert excinfo.value.status == 404
    assert excinfo.value.body == {"error": "sandbox not found"}
    assert "sandbox not found" in str(excinfo.value)


async def test_error_body_that_is_not_json_keeps_the_status(boot) -> None:
    client, _ = boot(lambda r: FakeResponse(status=502, text="upstream down"))
    with pytest.raises(VoraError) as excinfo:
        await client.sandboxes.get("sbx_1")
    assert excinfo.value.status == 502
    assert "upstream down" in str(excinfo.value)
