"""Sandbox behaviour: exec semantics, streaming, files and audit readers."""

from __future__ import annotations

import base64

import pytest

from vora import ExecChunk, VoraError

from conftest import FakeResponse


def exec_stream(*items: dict) -> FakeResponse:
    return FakeResponse(ndjson=list(items))


async def test_exec_runs_a_shell_line_and_aggregates_output(boot) -> None:
    def handler(request):
        if request.url.endswith("/exec"):
            return exec_stream(
                {"stdout": base64.b64encode(b"45\n").decode()},
                {"stderr": base64.b64encode(b"warn\n").decode()},
                {"exit": {"exitCode": 0, "reason": "EXITED"}},
            )
        return None

    client, api = boot(handler)
    sandbox = await client.sandboxes.create(image="vora/python")

    result = await sandbox.exec("python -c 'print(45)'", timeout_seconds=30)

    assert result.stdout == "45\n"
    assert result.stderr == "warn\n"
    assert result.exit_code == 0
    assert result.reason == "EXITED"
    assert api.last.json == {
        "command": "sh",
        "args": ["-c", "python -c 'print(45)'"],
        "timeoutSeconds": "30",
    }


async def test_exec_with_args_runs_literally(boot) -> None:
    def handler(request):
        if request.url.endswith("/exec"):
            return exec_stream({"exit": {"exitCode": 0}})
        return None

    client, api = boot(handler)
    sandbox = await client.sandboxes.create(image="vora/python")

    await sandbox.exec("echo", ["hi"], cwd="/workspace", env=["K=V"], stdin=b"payload")

    assert api.last.json == {
        "command": "echo",
        "args": ["hi"],
        "cwd": "/workspace",
        "env": ["K=V"],
        "stdin": base64.b64encode(b"payload").decode(),
    }


async def test_stream_exec_yields_chunks(boot) -> None:
    def handler(request):
        if request.url.endswith("/exec"):
            return exec_stream(
                {"stdout": base64.b64encode(b"a").decode()},
                {"exit": {"exitCode": 3, "reason": "OOM_KILLED"}},
            )
        return None

    client, _ = boot(handler)
    sandbox = await client.sandboxes.create(image="vora/python")

    chunks = [chunk async for chunk in sandbox.stream_exec("echo a")]

    assert chunks == [
        ExecChunk("stdout", data=b"a"),
        ExecChunk("exit", exit_code=3, reason="OOM_KILLED"),
    ]


async def test_killed_run_fails_loud_with_partial_output(boot) -> None:
    def handler(request):
        if request.url.endswith("/exec"):
            return exec_stream({"stdout": base64.b64encode(b"partial").decode()})
        return None

    client, _ = boot(handler)
    sandbox = await client.sandboxes.create(image="vora/python")

    with pytest.raises(VoraError, match="without an exit record") as excinfo:
        await sandbox.exec("long-running")

    assert excinfo.value.body == {"stdout": "partial", "stderr": ""}


async def test_files_round_trip_bytes_and_text(boot) -> None:
    def handler(request):
        if request.method == "PUT":
            return FakeResponse(json_body={"bytes_written": len(request.body)})
        if request.url.endswith("notes.txt"):
            return FakeResponse(bytes_body="hi".encode())
        if request.url.endswith("out.bin"):
            return FakeResponse(bytes_body=b"\x00\x01raw")
        return None

    client, api = boot(handler)
    sandbox = await client.sandboxes.create(image="vora/python")

    written = await sandbox.write_file("/workspace/out.bin", b"\x00\x01raw")
    assert written == 5
    assert api.last.method == "PUT"
    assert "path=%2Fworkspace%2Fout.bin" in api.last.url
    assert "create_parents=true" in api.last.url

    assert await sandbox.read_file("/workspace/out.bin") == b"\x00\x01raw"
    assert api.last.method == "GET"
    assert await sandbox.read_text("/workspace/notes.txt") == "hi"


async def test_stats_and_audit_readers(boot) -> None:
    def handler(request):
        if request.url.endswith("/stats"):
            return FakeResponse(json_body={"memoryBytes": "1024", "pids": 3, "oomKills": 1})
        if request.url.endswith("/events"):
            return FakeResponse(json_body={"events": [{"sandboxId": "sbx_test", "from": "REQUESTED", "to": "RUNNING"}]})
        if request.url.endswith("/execs"):
            return FakeResponse(json_body={"execs": [{"executionId": "exe_1", "exitCode": 0, "args": ["hi"]}]})
        if request.url.endswith("/egress"):
            return FakeResponse(json_body={"records": [{"sandboxId": "sbx_test", "allowed": False, "reason": "domain_not_allowed"}]})
        return None

    client, _ = boot(handler)
    sandbox = await client.sandboxes.create(image="vora/python")

    stats = await sandbox.stats()
    assert stats.memory_bytes == 1024
    assert stats.pids == 3
    assert stats.oom_kills == 1

    events = await sandbox.events()
    assert events[0].from_state == "REQUESTED"
    assert events[0].to_state == "RUNNING"

    execs = await sandbox.execs()
    assert execs[0].execution_id == "exe_1"
    assert execs[0].args == ("hi",)

    egress = await sandbox.egress()
    assert egress[0].allowed is False
