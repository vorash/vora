"""Snapshot capture, listing and forks."""

from __future__ import annotations

from conftest import FakeResponse


async def test_snapshot_lifecycle(boot) -> None:
    def handler(request):
        if request.url.endswith("/snapshot"):
            return FakeResponse(status=201, json_body={"id": "snap_1", "sandboxId": "sbx_test", "bytes": "4096"})
        if request.url.endswith("/fork"):
            return FakeResponse(status=201, json_body={"id": "sbx_fork", "state": "RUNNING"})
        if request.url.endswith("/v1/snapshots"):
            return FakeResponse(json_body={"snapshots": [{"id": "snap_1"}, {"id": "snap_2"}]})
        if request.url.endswith("/v1/snapshots/snap_1"):
            return FakeResponse(json_body={"id": "snap_1", "sandboxId": "sbx_test"})
        return None

    client, api = boot(handler)

    snap = await client.snapshots.create("sbx_test")
    assert snap.id == "snap_1"
    assert snap.info.sandbox_id == "sbx_test"
    assert api.last.method == "POST"

    snaps = await client.snapshots.list()
    assert [s.id for s in snaps] == ["snap_1", "snap_2"]

    refreshed = await snap.refresh()
    assert refreshed.id == "snap_1"

    fork = await snap.fork()
    assert fork.id == "sbx_fork"
    assert api.last.url.endswith("/v1/snapshots/snap_1/fork")
    assert api.last.method == "POST"

    await snap.remove()
    assert api.last.method == "DELETE"


async def test_fork_by_id_returns_a_sandbox(boot) -> None:
    client, _ = boot(lambda r: FakeResponse(status=201, json_body={"id": "sbx_fork", "state": "RUNNING"}))
    fork = await client.snapshots.fork("snap_1")
    assert fork.id == "sbx_fork"
