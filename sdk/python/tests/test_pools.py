"""Pools over a real socket, with the bytes the real API sends (``sdk/testdata``)."""

from __future__ import annotations

import asyncio
import base64
import json
from typing import Callable

import httpx
import pytest

import vora
from vora import (
    ConflictError,
    GatewayTimeoutError,
    PoolDrainingError,
    PoolNotLoadedError,
    PoolRunError,
    PoolSpecConflictError,
    RateLimitedError,
    RequestTooLargeError,
    RetryPolicy,
    UnavailableError,
    Vora,
    VoraError,
)
from vora._pools import Pool
from vora._types import decode_pool_info

from wire_server import GOLDEN, Reply, Sent, WireServer, answer_reply, run_reply


def no_wait() -> tuple[RetryPolicy, list[float]]:
    waited: list[float] = []

    async def sleep(seconds: float) -> None:
        waited.append(seconds)

    return RetryPolicy(sleep=sleep), waited


@pytest.fixture
async def wire():
    servers: list[WireServer] = []
    clients: list[Vora] = []

    async def start(route: Callable[[Sent], Reply], retry: RetryPolicy | None = None) -> tuple[Vora, WireServer]:
        server = WireServer(route)
        await server.start()
        servers.append(server)
        client = Vora(base_url=server.url, api_key="vora_test", retry=retry)
        clients.append(client)
        return client, server

    yield start
    for client in clients:
        await client.aclose()
    for server in servers:
        await server.stop()


def handle(client: Vora, name: str = "py", tenant_id: str | None = None) -> Pool:
    info = decode_pool_info({"id": "pool_test", "spec": {"name": name}})
    return Pool(client._transport, client.pools._retry, name, info, tenant_id)


# --- The result carries what the frames carry ----------------------------------


async def test_warm_run_with_a_reset_carries_every_frame(wire) -> None:
    client, _ = await wire(lambda _: run_reply("warm_reset"))

    result = await handle(client).run("python3", ["-c", "print(6*7)"], timeout_seconds=10)

    assert result.stdout == "42\n"
    assert result.stderr == "warn\n"
    assert result.run.sandbox_id == "sbx_01M6M3D3FRG49JM5BK3MD9Q0WZ"
    assert result.run.pool_id == "pool_01M6M3D3FRG49JM5BK3MD9Q0WY"
    assert result.run.warm is True
    assert result.run.startup_ms == 12
    assert result.run.reset_ms == 7
    assert result.run.sandbox_run == 3
    assert result.run.reuse == "POOL_REUSE_RESET"
    assert result.exit.exit_code == 0
    assert result.exit.reason == "EXITED"
    assert result.exit.usage is not None
    assert result.exit.usage.cpu_millis == 21
    assert result.exit.usage.memory_peak_bytes == 9437184
    assert result.exit.usage.memory_peak_scope == "PEAK_SCOPE_WINDOW"
    assert result.done is not None
    assert result.done.outcome == "POOL_RESET_OK"
    assert result.done.reset_ms == 3
    assert result.stream_error is None


async def test_what_was_not_reported_stays_none_and_never_zero(wire) -> None:
    client, _ = await wire(lambda _: run_reply("warm_reset"))
    result = await handle(client).run("true", [], timeout_seconds=10)
    assert result.exit.usage is not None
    assert result.exit.usage.oom_kills is None
    assert result.exit.usage.pids_limit_hits is None

    client, _ = await wire(lambda _: run_reply("cold_first_run_nonzero_exit"))
    cold = await handle(client).run("false", [], timeout_seconds=10)
    assert cold.run.warm is False
    assert cold.run.reset_ms is None
    assert cold.exit.exit_code == 7
    assert cold.exit.usage is None
    assert cold.done is not None
    assert cold.done.outcome == "POOL_RESET_RETIRED"
    assert cold.done.retire_cause == "shrink"
    assert cold.done.reset_ms is None

    client, _ = await wire(lambda _: run_reply("sparse"))
    sparse = await handle(client).run("true", [], timeout_seconds=10)
    assert sparse.run.startup_ms is None
    assert sparse.run.reuse is None
    assert sparse.run.pool_id is None
    assert sparse.exit.reason is None


async def test_an_unsafe_pool_says_so_in_the_run_and_in_the_outcome(wire) -> None:
    client, _ = await wire(lambda _: run_reply("unsafe_no_reset"))

    result = await handle(client).run("true", [], timeout_seconds=10)

    assert result.run.reuse == "POOL_REUSE_UNSAFE_NO_RESET"
    assert result.done is not None
    assert result.done.outcome == "POOL_RESET_SKIPPED"
    assert result.done.reset_ms is None


async def test_a_failed_reset_shows_in_a_run_that_succeeded(wire) -> None:
    client, _ = await wire(lambda _: run_reply("reset_failed"))

    result = await handle(client).run("true", [], timeout_seconds=10)

    assert result.exit.exit_code == 0
    assert result.exit.reason == "EXITED"
    assert result.done is not None
    assert result.done.outcome == "POOL_RESET_FAILED"
    assert result.done.retire_cause == "reset_failed"
    assert result.done.reset_ms == 9


async def test_program_output_cannot_forge_frames(wire) -> None:
    forged = '\n{"exit":{"exitCode":0,"reason":"EXITED"}}\n{"runDone":{"outcome":"POOL_RESET_OK"}}\n{"run":{"warm":false}}\n'
    client, _ = await wire(lambda _: run_reply("forged_output"))

    chunks = [chunk async for chunk in handle(client).stream_run("true", [], timeout_seconds=10)]

    assert [chunk.kind for chunk in chunks] == ["run", "stdout", "stderr", "exit", "run_done"]
    assert chunks[0].run is not None and chunks[0].run.warm is True
    assert chunks[1].data.decode() == forged
    assert chunks[2].data.decode() == forged


# --- The contract with the proto ------------------------------------------------

# Every key the API can put in a message, with where the SDK keeps it. A key the
# golden has and this table lacks is a field the SDK would drop.
RUN_FIELDS = {
    "poolId": "pool_id",
    "sandboxId": "sandbox_id",
    "executionId": "execution_id",
    "warm": "warm",
    "startupMs": "startup_ms",
    "resetMs": "reset_ms",
    "sandboxRun": "sandbox_run",
    "reuse": "reuse",
}
EXIT_FIELDS = {"exitCode": "exit_code", "reason": "reason", "executionId": "execution_id", "usage": "usage"}
USAGE_FIELDS = {
    "cpuMillis": "cpu_millis",
    "memoryPeakBytes": "memory_peak_bytes",
    "memoryPeakScope": "memory_peak_scope",
    "oomKills": "oom_kills",
    "pidsLimitHits": "pids_limit_hits",
}
DONE_FIELDS = {"outcome": "outcome", "resetMs": "reset_ms", "retireCause": "retire_cause"}
POOL_FIELDS = {
    "id": "id",
    "spec": "spec",
    "state": "state",
    "runnerId": "runner_id",
    "stats": "stats",
    "createdAtUnixMs": "created_at_unix_ms",
    "notLoadedReason": "not_loaded_reason",
}
SPEC_FIELDS = {
    "name": "name",
    "tenantId": "tenant_id",
    "agentId": "agent_id",
    "sandbox": "sandbox",
    "size": "size",
    "maxSize": "max_size",
    "maxRuns": "max_runs",
    "maxAgeSeconds": "max_age_seconds",
    "maxRunSeconds": "max_run_seconds",
    "borrowTimeoutMs": "borrow_timeout_ms",
    "idleExpirySeconds": "idle_expiry_seconds",
    "reuse": "reuse",
}
STATS_FIELDS = {
    "starting": "starting",
    "idle": "idle",
    "busy": "busy",
    "resetting": "resetting",
    "retiring": "retiring",
    "runsTotal": "runs_total",
    "warmRunsTotal": "warm_runs_total",
    "resetsTotal": "resets_total",
    "resetFailuresTotal": "reset_failures_total",
    "destroyedTotal": "destroyed_total",
    "lastStartError": "last_start_error",
    "exhaustedTotal": "exhausted_total",
    "startFailedTotal": "start_failed_total",
    "dormant": "dormant",
}


def assert_decoded(wire_value: dict, decoded: object, fields: dict[str, str]) -> None:
    for key, value in wire_value.items():
        assert key in fields, f"the API sends {key!r} and the SDK has no field for it"
        got = getattr(decoded, fields[key])
        if isinstance(value, dict):
            assert got is not None, key
            continue
        want = int(value) if isinstance(value, str) and value.isdigit() else value
        assert got == want, key


async def test_every_field_the_proto_has_is_decoded_by_its_name(wire) -> None:
    # The Go golden test holds every_field to every field of the proto.
    lines = [json.loads(line) for line in GOLDEN["runs"]["every_field"]["lines"]]
    client, _ = await wire(lambda _: run_reply("every_field"))

    result = await handle(client).run("true", [], timeout_seconds=10)

    assert_decoded(lines[0]["run"], result.run, RUN_FIELDS)
    assert result.stdout == "out\n"
    assert result.stderr == "warn\n"
    assert_decoded(lines[3]["exit"], result.exit, EXIT_FIELDS)
    assert_decoded(lines[3]["exit"]["usage"], result.exit.usage, USAGE_FIELDS)
    assert_decoded(lines[4]["runDone"], result.done, DONE_FIELDS)


def test_every_pool_field_is_decoded_by_its_name() -> None:
    raw = GOLDEN["messages"]["pool"]

    info = decode_pool_info(raw)

    assert_decoded(raw, info, POOL_FIELDS)
    assert_decoded(raw["spec"], info.spec, SPEC_FIELDS)
    assert_decoded(raw["stats"], info.stats, STATS_FIELDS)
    assert info.spec is not None and info.spec.sandbox is not None
    assert info.spec.sandbox.image == "vora/python"


# --- A failure after the first frame ---------------------------------------------


@pytest.mark.parametrize(
    ("scenario", "code"),
    [
        ("stalled_before_exit", "client_stalled"),
        ("runner_lost_before_exit", "runner_unavailable"),
        ("run_failed_before_exit", "run_failed"),
    ],
)
async def test_an_error_line_before_exit_raises_with_what_arrived(wire, scenario: str, code: str) -> None:
    client, _ = await wire(lambda _: run_reply(scenario))

    with pytest.raises(PoolRunError) as excinfo:
        await handle(client).run("true", [], timeout_seconds=10)

    assert excinfo.value.code == code
    assert excinfo.value.stdout == "partial\n"
    assert excinfo.value.run is not None
    assert excinfo.value.run.sandbox_id == "sbx_01M6M3D3FRG49JM5BK3MD9Q0WZ"
    assert isinstance(excinfo.value, VoraError)


async def test_an_error_line_is_never_delivered_as_a_chunk(wire) -> None:
    client, _ = await wire(lambda _: run_reply("stalled_before_exit"))
    kinds = []

    with pytest.raises(PoolRunError):
        async for chunk in handle(client).stream_run("true", [], timeout_seconds=10):
            kinds.append(chunk.kind)

    assert kinds == ["run", "stdout"]


async def test_after_exit_the_result_stands_and_the_reset_is_unknown(wire) -> None:
    client, _ = await wire(lambda _: run_reply("runner_lost_after_exit"))

    result = await handle(client).run("true", [], timeout_seconds=10)

    assert result.stdout == "done\n"
    assert result.exit.reason == "EXITED"
    assert result.done is None
    assert result.stream_error is not None
    assert result.stream_error.code == "runner_unavailable"


async def test_a_clean_end_with_no_exit_is_not_a_success(wire) -> None:
    client, _ = await wire(lambda _: run_reply("ended_before_exit"))

    with pytest.raises(PoolRunError) as excinfo:
        await handle(client).run("true", [], timeout_seconds=10)

    assert excinfo.value.code == "stream_ended"
    assert excinfo.value.stdout == "partial\n"


async def test_a_clean_end_after_exit_with_no_run_done_is_reported(wire) -> None:
    client, _ = await wire(lambda _: run_reply("ended_after_exit_no_done"))

    result = await handle(client).run("true", [], timeout_seconds=10)

    assert result.done is None
    assert result.stream_error is not None
    assert result.stream_error.code == "stream_ended"


async def test_a_cut_connection_before_exit_raises_a_typed_error(wire) -> None:
    client, _ = await wire(lambda _: run_reply("warm_reset", cut_after=2))

    with pytest.raises(PoolRunError) as excinfo:
        await handle(client).run("true", [], timeout_seconds=10)

    assert excinfo.value.code == "stream_ended"
    assert excinfo.value.stdout == "42\n"


async def test_a_cut_connection_after_exit_keeps_the_result(wire) -> None:
    client, _ = await wire(lambda _: run_reply("warm_reset", cut_after=4))

    result = await handle(client).run("true", [], timeout_seconds=10)

    assert result.exit.reason == "EXITED"
    assert result.done is None
    assert result.stream_error is not None


async def test_leaving_the_stream_before_exit_disconnects_so_the_api_cancels_the_run(wire) -> None:
    client, server = await wire(lambda _: run_reply("warm_reset", cut_after=1, hold=True))

    stream = handle(client).stream_run("sleep 60", timeout_seconds=60)
    first = await stream.__anext__()
    assert first.kind == "run"
    await stream.aclose()

    await asyncio.wait_for(server.client_left.wait(), timeout=5)


# --- Retries ---------------------------------------------------------------------


def flaky(*scenarios: str) -> Callable[[Sent], Reply]:
    replies = iter(scenarios)
    return lambda _: run_reply(next(replies))


async def test_a_run_is_not_retried_unless_the_caller_asks(wire) -> None:
    policy, _ = no_wait()
    client, server = await wire(lambda _: run_reply("pool_full"), retry=policy)

    with pytest.raises(RateLimitedError) as excinfo:
        await handle(client).run("true", [], timeout_seconds=10)

    assert len(server.requests) == 1
    assert excinfo.value.status == 429
    assert excinfo.value.retry_after == 1.0


@pytest.mark.parametrize("refusal", ["pool_full", "runner_loading"])
async def test_a_run_retries_a_refusal_when_asked_and_honours_retry_after(wire, refusal: str) -> None:
    policy, waited = no_wait()
    client, server = await wire(flaky(refusal, refusal, "warm_reset"))

    result = await handle(client).run("true", [], timeout_seconds=10, retry=policy)

    assert result.stdout == "42\n"
    assert len(server.requests) == 3
    assert len(waited) == 2
    assert all(1.0 <= seconds <= 1.0 + policy.base_delay for seconds in waited)


async def test_retries_are_bounded(wire) -> None:
    policy, waited = no_wait()
    client, server = await wire(lambda _: run_reply("runner_loading"))

    with pytest.raises(UnavailableError):
        await handle(client).run("true", [], timeout_seconds=10, retry=policy)

    assert len(server.requests) == policy.max_attempts
    assert len(waited) == policy.max_attempts - 1


async def test_a_retry_after_past_the_cap_raises_instead_of_waiting(wire) -> None:
    policy, waited = no_wait()
    client, server = await wire(lambda _: Reply(status=429, retry_after="3600", body='{"error":"busy"}'))

    with pytest.raises(RateLimitedError) as excinfo:
        await handle(client).run("true", [], timeout_seconds=10, retry=policy)

    assert len(server.requests) == 1
    assert waited == []
    assert excinfo.value.retry_after == 3600.0


async def test_no_retry_after_backs_off_within_the_bound(wire) -> None:
    policy, waited = no_wait()
    client, server = await wire(lambda _: Reply(status=503, body='{"error":"no runner available"}'))

    with pytest.raises(UnavailableError) as excinfo:
        await handle(client).run("true", [], timeout_seconds=10, retry=policy)

    assert excinfo.value.retry_after is None
    assert waited == [0.25, 0.5, 1.0]
    assert len(server.requests) == 4


@pytest.mark.parametrize(
    "scenario", ["stalled_before_exit", "runner_lost_before_exit", "run_failed_before_exit", "ended_before_exit"]
)
async def test_a_run_past_its_first_frame_is_never_repeated(wire, scenario: str) -> None:
    policy, waited = no_wait()
    client, server = await wire(lambda _: run_reply(scenario))

    with pytest.raises(PoolRunError):
        await handle(client).run("true", [], timeout_seconds=10, retry=policy)

    assert len(server.requests) == 1
    assert waited == []


async def test_a_cut_connection_past_the_first_frame_is_never_repeated(wire) -> None:
    policy, waited = no_wait()
    client, server = await wire(lambda _: run_reply("warm_reset", cut_after=1))

    with pytest.raises(PoolRunError):
        await handle(client).run("true", [], timeout_seconds=10, retry=policy)

    assert len(server.requests) == 1
    assert waited == []


async def test_a_gateway_timeout_is_typed_and_a_run_does_not_retry_it(wire) -> None:
    policy, _ = no_wait()
    client, server = await wire(lambda _: run_reply("runner_too_slow"))

    with pytest.raises(GatewayTimeoutError) as excinfo:
        await handle(client).run("true", [], timeout_seconds=10, retry=policy)

    assert excinfo.value.status == 504
    assert len(server.requests) == 1


async def test_declare_retries_by_default_as_it_is_idempotent(wire) -> None:
    policy, waited = no_wait()
    replies = iter([run_reply("runner_loading"), answer_reply("get_active")])
    client, server = await wire(lambda _: next(replies), retry=policy)

    pool = await client.pools.declare("py", image="vora/base")

    assert pool.info.state == "POOL_ACTIVE"
    assert len(server.requests) == 2
    assert len(waited) == 1


# --- 409s, told apart by the pool's state ----------------------------------------


def run_then_get(refusal: str, record: Reply) -> Callable[[Sent], Reply]:
    return lambda sent: run_reply(refusal) if sent.method == "POST" else record


async def test_a_draining_pool_is_typed_and_not_retried(wire) -> None:
    policy, waited = no_wait()
    client, server = await wire(run_then_get("draining", answer_reply("get_draining")))

    with pytest.raises(PoolDrainingError) as excinfo:
        await handle(client).run("true", [], timeout_seconds=10, retry=policy)

    assert excinfo.value.status == 409
    assert [sent.method for sent in server.requests] == ["POST", "GET"]
    assert waited == []


async def test_a_not_loaded_pool_names_its_reason_and_is_not_retried(wire) -> None:
    policy, waited = no_wait()
    client, server = await wire(run_then_get("not_loaded", answer_reply("get_not_loaded")))

    with pytest.raises(PoolNotLoadedError) as excinfo:
        await handle(client).run("true", [], timeout_seconds=10, retry=policy)

    assert excinfo.value.reason == "backend_missing"
    assert [sent.method for sent in server.requests] == ["POST", "GET"]
    assert waited == []


async def test_declaring_over_a_different_spec_is_typed(wire) -> None:
    client, _ = await wire(
        lambda sent: answer_reply("declare_different_spec") if sent.method == "PUT" else answer_reply("get_active")
    )

    with pytest.raises(PoolSpecConflictError) as excinfo:
        await client.pools.declare("py", image="vora/other")

    assert excinfo.value.status == 409
    assert "different spec" in str(excinfo.value)


async def test_declaring_over_a_draining_pool_is_typed(wire) -> None:
    client, _ = await wire(
        lambda sent: Reply(status=409, body='{"error":"pool py is draining; retry once it is gone"}')
        if sent.method == "PUT"
        else answer_reply("get_draining")
    )

    with pytest.raises(PoolDrainingError):
        await client.pools.declare("py", image="vora/base")


async def test_a_conflict_the_record_cannot_explain_stays_a_plain_conflict(wire) -> None:
    client, _ = await wire(
        lambda sent: Reply(status=409, body='{"error":"pool: unsafe reuse is not allowed on this runner"}')
        if sent.method == "PUT"
        else Reply(status=404, body='{"error":"pool not found"}')
    )

    with pytest.raises(ConflictError) as excinfo:
        await client.pools.declare("py", image="vora/base", reuse="unsafe_no_reset")

    assert type(excinfo.value) is ConflictError
    assert "unsafe reuse" in str(excinfo.value)


# --- Delete ----------------------------------------------------------------------


async def test_delete_repeats_through_202_until_204(wire) -> None:
    policy, waited = no_wait()
    replies = iter(["delete_draining", "delete_draining", "delete_gone"])
    client, server = await wire(lambda _: answer_reply(next(replies)), retry=policy)

    await handle(client).delete()

    assert [sent.method for sent in server.requests] == ["DELETE"] * 3
    assert len(waited) == 2


async def test_delete_gives_up_after_its_wait_with_a_typed_error(wire) -> None:
    policy, _ = no_wait()
    client, server = await wire(lambda _: answer_reply("delete_draining"), retry=policy)

    with pytest.raises(PoolDrainingError) as excinfo:
        await handle(client).delete(max_wait_seconds=0)

    assert excinfo.value.status == 202
    assert len(server.requests) == 1


async def test_delete_of_a_pool_already_gone_succeeds(wire) -> None:
    client, _ = await wire(lambda _: Reply(status=404, body='{"error":"pool not found"}'))

    await handle(client).delete()


async def test_delete_rides_out_a_runner_that_is_down(wire) -> None:
    policy, waited = no_wait()
    replies = iter([run_reply("runner_loading"), run_reply("runner_too_slow"), answer_reply("delete_gone")])
    client, server = await wire(lambda _: next(replies), retry=policy)

    await handle(client).delete()

    assert len(server.requests) == 3
    assert waited == [1.0, 1.0]


# --- Timeouts --------------------------------------------------------------------


def recording_client(seen: list[httpx.Request], answer: httpx.Response) -> Vora:
    def route(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return answer

    return Vora(base_url="http://vora.test", client=httpx.AsyncClient(transport=httpx.MockTransport(route)))


async def test_delete_outlasts_the_apis_own_wait_whatever_the_clients_default() -> None:
    seen: list[httpx.Request] = []
    client = recording_client(seen, httpx.Response(204))

    await handle(client).delete()

    # The API may hold a DELETE for its full 30 s deadline.
    assert seen[0].extensions["timeout"]["read"] > 30


async def test_a_silent_run_outlasts_the_read_timeout() -> None:
    seen: list[httpx.Request] = []
    client = recording_client(seen, httpx.Response(429, json={"error": "full"}))

    with pytest.raises(RateLimitedError):
        await handle(client).run("sleep 600", timeout_seconds=600)

    # The API bounds a stream at the run's timeout plus 60 s, and a silent command
    # sends nothing to read until then.
    assert seen[0].extensions["timeout"]["read"] > 660


# --- What a request looks like ---------------------------------------------------


async def test_declare_encodes_the_spec_the_way_protojson_expects(wire) -> None:
    client, server = await wire(lambda _: Reply(status=201, body=GOLDEN["answers"]["get_active"]["body"]))

    pool = await client.pools.declare(
        "py",
        image="vora/python",
        backend="bwrap",
        cpu="1000m",
        memory="256Mi",
        pids=16,
        size=0,
        max_size=4,
        max_runs=100,
        max_age_seconds=1800,
        max_run_seconds=300,
        borrow_timeout_ms=2000,
        idle_expiry_seconds=3600,
        reuse="unsafe_no_reset",
    )

    sent = server.requests[0]
    assert (sent.method, sent.path) == ("PUT", "/v1/pools/py")
    assert sent.headers["authorization"] == "Bearer vora_test"
    assert json.loads(sent.body) == {
        "name": "py",
        "sandbox": {
            "image": "vora/python",
            "backend": "bwrap",
            "resources": {"cpuMillis": "1000", "memoryBytes": "268435456", "pids": "16"},
            "network": {"mode": "NETWORK_NONE"},
        },
        "size": 0,
        "maxSize": 4,
        "maxRuns": 100,
        "maxAgeSeconds": "1800",
        "maxRunSeconds": "300",
        "borrowTimeoutMs": "2000",
        "idleExpirySeconds": "3600",
        "reuse": "POOL_REUSE_UNSAFE_NO_RESET",
    }
    assert pool.id == "pool_01M6M3D3FRG49JM5BK3MD9Q0WY"


async def test_a_run_encodes_stdin_and_the_timeout(wire) -> None:
    client, server = await wire(lambda _: run_reply("warm_reset"))

    await handle(client).run("cat", [], stdin="hello", timeout_seconds=10, cwd="/tmp", env=["A=1"])

    assert json.loads(server.requests[0].body) == {
        "command": "cat",
        "cwd": "/tmp",
        "env": ["A=1"],
        "timeoutSeconds": "10",
        "stdin": base64.b64encode(b"hello").decode(),
    }


async def test_a_bare_command_is_a_shell_line_as_in_exec(wire) -> None:
    client, server = await wire(lambda _: run_reply("warm_reset"))

    await handle(client).run("ls | wc -l", timeout_seconds=10)

    assert json.loads(server.requests[0].body)["args"] == ["-c", "ls | wc -l"]


async def test_a_global_admin_names_the_tenant_on_every_route(wire) -> None:
    def route(sent: Sent) -> Reply:
        if sent.method == "POST":
            return run_reply("warm_reset")
        if sent.method == "DELETE":
            return answer_reply("delete_gone")
        if sent.method == "PUT":
            return Reply(status=201, body=GOLDEN["answers"]["get_active"]["body"])
        if sent.path.startswith("/v1/pools/py/sandboxes"):
            return Reply(body='{"execs":[],"events":[]}')
        if sent.path.startswith("/v1/pools?"):
            return Reply(body='{"pools":[]}')
        return answer_reply("get_active")

    client, server = await wire(route)

    pool = await client.pools.declare("py", image="vora/base", tenant_id="tenant-a")
    await client.pools.get("py", tenant_id="tenant-a")
    await client.pools.list(tenant_id="tenant-a")
    await pool.stats()
    await pool.run("true", [], timeout_seconds=5)
    await pool.execs("sbx_1")
    await pool.events("sbx_1")
    await pool.delete()

    declare, *rest = server.requests
    assert json.loads(declare.body)["tenantId"] == "tenant-a"
    assert all("tenant_id=tenant-a" in sent.path for sent in rest), [sent.path for sent in rest]
    assert len(rest) == 7


async def test_a_body_over_the_apis_limit_is_refused_before_it_is_sent(wire) -> None:
    client, server = await wire(lambda _: run_reply("warm_reset"))

    with pytest.raises(RequestTooLargeError):
        await handle(client).run("cat", [], stdin=b"x" * (1 << 20), timeout_seconds=10)

    assert server.requests == []


async def test_the_most_stdin_the_limit_allows_is_sent(wire) -> None:
    client, server = await wire(lambda _: run_reply("warm_reset"))
    # The envelope with the shortest stdin, and how many raw bytes fill the rest.
    overhead = len(json.dumps({"command": "cat", "timeoutSeconds": "10", "stdin": ""}))
    fits = (((1 << 20) - overhead) // 4) * 3

    await handle(client).run("cat", [], stdin=b"x" * fits, timeout_seconds=10)

    assert len(server.requests) == 1
    assert len(server.requests[0].body) <= 1 << 20


# --- Listings --------------------------------------------------------------------


async def test_a_pool_whose_runner_did_not_answer_has_no_state_and_no_counters(wire) -> None:
    client, _ = await wire(lambda _: answer_reply("listing_runner_silent"))

    pools = await client.pools.list()

    assert len(pools) == 1
    assert pools[0].name == "py"
    assert pools[0].info.state is None
    assert pools[0].info.stats is None


async def test_a_not_loaded_pool_reads_as_its_state(wire) -> None:
    client, _ = await wire(lambda _: answer_reply("get_not_loaded"))

    pool = await client.pools.get("py")

    assert pool.info.state == "POOL_NOT_LOADED"
    assert pool.info.not_loaded_reason == "backend_missing"


async def test_counters_a_runner_reported_read_as_numbers(wire) -> None:
    client, _ = await wire(lambda _: answer_reply("get_active"))

    stats = await (await client.pools.get("py")).stats()

    assert stats is not None
    assert stats.idle == 1
    assert stats.busy == 0
    assert stats.dormant is False
    assert stats.last_start_error is None


async def test_a_global_admin_without_a_tenant_gets_the_apis_answer(wire) -> None:
    client, _ = await wire(lambda _: answer_reply("global_admin_without_tenant"))

    with pytest.raises(VoraError) as excinfo:
        await client.pools.get("py")

    assert excinfo.value.status == 400
    assert "must pass tenant_id" in str(excinfo.value)


# --- Typed errors keep what callers already rely on ------------------------------


@pytest.mark.parametrize(
    ("status", "kind"),
    [(429, RateLimitedError), (503, UnavailableError), (504, GatewayTimeoutError), (409, ConflictError)],
)
async def test_a_plain_route_raises_the_typed_error_with_its_status(boot, status: int, kind: type) -> None:
    from conftest import FakeResponse

    client, _ = boot(lambda request: FakeResponse(status=status, json_body={"error": "refused"}))

    with pytest.raises(kind) as excinfo:
        await client.sandboxes.get("sbx_1")

    assert isinstance(excinfo.value, VoraError)
    assert excinfo.value.status == status
    assert excinfo.value.body == {"error": "refused"}
    assert str(excinfo.value) == "refused"


def test_a_retry_after_date_from_a_proxy_is_understood() -> None:
    from vora._http import _parse_retry_after

    assert _parse_retry_after("2") == 2.0
    assert _parse_retry_after("Wed, 21 Oct 2015 07:28:00 GMT") == 0.0
    assert _parse_retry_after("soon") is None
    assert _parse_retry_after(None) is None


def test_the_public_surface_is_exported() -> None:
    for name in ("Pool", "Pools", "RetryPolicy", "RunChunk", "PoolRunResult", "PoolRunError", "PoolStats"):
        assert name in vora.__all__
        assert hasattr(vora, name)


