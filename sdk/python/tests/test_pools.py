"""Pools over a real socket, with the bytes the real API sends (``sdk/testdata``)."""

from __future__ import annotations

import asyncio
import base64
import contextlib
import json
from typing import Any, Awaitable, Callable

import httpx
import pytest

import vora
from vora import (
    ConflictError,
    GatewayTimeoutError,
    PoolDrainingError,
    PoolNotLoadedError,
    PoolNotOnRunnerError,
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
from vora._sandbox import Sandbox, exec_request
from vora._types import SandboxInfo, decode_pool_info, decode_sandbox_exec, decode_sandbox_info

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


LIMITS = GOLDEN["limits"]


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

    async for chunk in handle(client).stream_run("sleep 60", timeout_seconds=60):
        assert chunk.kind == "run"
        break

    await asyncio.wait_for(server.client_left.wait(), timeout=5)


def remembering_responses(opened: list[httpx.Response]) -> httpx.AsyncClient:
    async def remember(response: httpx.Response) -> None:
        opened.append(response)

    return httpx.AsyncClient(event_hooks={"response": [remember]})


async def test_breaking_out_with_the_stream_still_held_closes_the_response() -> None:
    # A stream kept in a variable must not hold the connection, and with it the pool
    # member, until the variable goes. Dropping httpx's generators closes the socket on
    # their own, so only `is_closed` says the SDK closed its response.
    opened: list[httpx.Response] = []
    server = WireServer(lambda _: run_reply("warm_reset", cut_after=1, hold=True))
    await server.start()
    async with remembering_responses(opened) as http:
        client = Vora(base_url=server.url, client=http)
        stream = handle(client).stream_run("sleep 60", timeout_seconds=60)
        async for chunk in stream:
            assert chunk.kind == "run"
            break

        await asyncio.wait_for(server.client_left.wait(), timeout=5)

        assert opened[0].is_closed
        assert stream is not None
    await server.stop()


async def test_aclosing_the_iterator_closes_the_response_before_it_returns() -> None:
    opened: list[httpx.Response] = []
    server = WireServer(lambda _: run_reply("warm_reset", cut_after=1, hold=True))
    await server.start()
    async with remembering_responses(opened) as http:
        client = Vora(base_url=server.url, client=http)
        chunks = aiter(handle(client).stream_run("sleep 60", timeout_seconds=60))
        async with contextlib.aclosing(chunks):
            async for _ in chunks:
                break
            assert not opened[0].is_closed

        assert opened[0].is_closed
        await asyncio.wait_for(server.client_left.wait(), timeout=5)
    await server.stop()


async def test_a_stream_runs_its_command_once(wire) -> None:
    client, server = await wire(lambda _: run_reply("warm_reset"))
    stream = handle(client).stream_run("true", [], timeout_seconds=10)
    assert [chunk.kind async for chunk in stream][-1] == "run_done"

    with pytest.raises(VoraError):
        aiter(stream)

    assert len(server.requests) == 1


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

    pool = await client.pools.declare("py", image="vora/base", cpu="100m", memory="1Mi")

    assert pool.info.state == "POOL_ACTIVE"
    assert len(server.requests) == 2
    assert len(waited) == 1


# --- 409s, told apart by the pool's state ----------------------------------------


def run_then_get(refusal: str, record: Reply) -> Callable[[Sent], Reply]:
    return lambda sent: run_reply(refusal) if sent.method == "POST" else record


async def test_a_run_refused_on_an_active_pool_stays_a_plain_conflict(wire) -> None:
    # A spec conflict is a declare's answer, and a run has no spec to differ.
    client, _ = await wire(lambda sent: run_reply("draining") if sent.method == "POST" else answer_reply("get_active"))

    with pytest.raises(ConflictError) as excinfo:
        await handle(client).run("true", [], timeout_seconds=10)

    assert type(excinfo.value) is ConflictError
    assert excinfo.value.status == 409


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
        await client.pools.declare("py", image="vora/other", cpu="100m", memory="1Mi")

    assert excinfo.value.status == 409
    assert "different spec" in str(excinfo.value)


async def test_declaring_over_a_draining_pool_is_typed(wire) -> None:
    client, _ = await wire(
        lambda sent: Reply(status=409, body='{"error":"pool py is draining; retry once it is gone"}')
        if sent.method == "PUT"
        else answer_reply("get_draining")
    )

    with pytest.raises(PoolDrainingError):
        await client.pools.declare("py", image="vora/base", cpu="100m", memory="1Mi")


async def test_a_conflict_the_record_cannot_explain_stays_a_plain_conflict(wire) -> None:
    client, _ = await wire(
        lambda sent: Reply(status=409, body='{"error":"pool: unsafe reuse is not allowed on this runner"}')
        if sent.method == "PUT"
        else Reply(status=404, body='{"error":"pool not found"}')
    )

    with pytest.raises(ConflictError) as excinfo:
        await client.pools.declare("py", image="vora/base", cpu="100m", memory="1Mi", reuse="unsafe_no_reset")

    assert type(excinfo.value) is ConflictError
    assert "unsafe reuse" in str(excinfo.value)


# --- A pool its runner lost ------------------------------------------------------


async def test_a_lost_pool_reads_as_its_state_with_nothing_measured(wire) -> None:
    client, _ = await wire(lambda _: answer_reply("get_not_on_runner"))

    pool = await client.pools.get("py")

    assert pool.info.state == "POOL_NOT_ON_RUNNER"
    assert pool.info.stats is None
    assert await pool.stats() is None
    assert pool.info.spec is not None
    assert pool.info.spec.name == "py"
    assert pool.info.spec.sandbox is None
    assert pool.info.created_at_unix_ms is None


async def test_a_run_on_a_lost_pool_is_typed_and_never_retried(wire) -> None:
    policy, waited = no_wait()
    client, server = await wire(
        lambda sent: answer_reply("run_not_on_runner") if sent.method == "POST" else answer_reply("get_not_on_runner")
    )

    with pytest.raises(PoolNotOnRunnerError) as excinfo:
        await handle(client).run("true", [], timeout_seconds=10, retry=policy)

    assert isinstance(excinfo.value, ConflictError)
    assert excinfo.value.status == 409
    assert "declare it again" in str(excinfo.value)
    assert [sent.method for sent in server.requests] == ["POST", "GET"]
    assert waited == []


async def test_redeclaring_a_lost_pool_moves_to_the_new_id_and_no_id_is_cached(wire) -> None:
    answers = {"PUT": answer_reply("declare_lost_pool"), "POST": run_reply("warm_reset")}
    reads = iter([answer_reply("get_not_on_runner"), answer_reply("declare_lost_pool")])
    client, server = await wire(lambda sent: answers.get(sent.method) or next(reads))

    held = await client.pools.get("py")
    old_id = held.id

    declared = await client.pools.declare("py", image="vora/base", cpu="100m", memory="1Mi")
    assert declared.id != old_id
    assert declared.info.state == "POOL_ACTIVE"

    # The handle from before the declare still reaches the pool, as it goes by name.
    await held.run("true", [], timeout_seconds=10)
    assert await held.refresh() == declared.info
    assert held.id == declared.id

    assert all(old_id not in sent.path and declared.id not in sent.path for sent in server.requests)


async def test_a_conflict_on_declaring_a_lost_pool_stays_a_plain_conflict(wire) -> None:
    client, _ = await wire(
        lambda sent: Reply(status=409, body='{"error":"pool id is taken"}')
        if sent.method == "PUT"
        else answer_reply("get_not_on_runner")
    )

    with pytest.raises(ConflictError) as excinfo:
        await client.pools.declare("py", image="vora/base", cpu="100m", memory="1Mi")

    assert type(excinfo.value) is ConflictError


# --- What declare refuses --------------------------------------------------------


async def test_a_declare_cannot_leave_out_cpu_or_memory(wire) -> None:
    client, server = await wire(lambda _: answer_reply("declare_no_resources"))

    with pytest.raises(TypeError):
        await client.pools.declare("py", image="vora/base")  # type: ignore[call-arg]
    assert server.requests == []

    with pytest.raises(VoraError) as excinfo:
        await client.pools.declare("py", image="vora/base", cpu=None, memory=None)  # type: ignore[arg-type]
    assert excinfo.value.status == 400
    assert "cpu_millis and memory_bytes" in str(excinfo.value)


async def test_a_borrow_timeout_past_the_cap_is_the_apis_400(wire) -> None:
    client, server = await wire(lambda _: answer_reply("declare_borrow_too_long"))

    with pytest.raises(VoraError) as excinfo:
        await client.pools.declare(
            "py", image="vora/base", cpu="100m", memory="1Mi", borrow_timeout_ms=LIMITS["borrowTimeoutMaxMs"] + 1
        )

    assert excinfo.value.status == 400
    assert "borrow_timeout" in str(excinfo.value)
    assert json.loads(server.requests[0].body)["borrowTimeoutMs"] == "300001"


async def test_another_agents_pool_name_is_a_plain_404_with_no_further_calls(wire) -> None:
    policy, waited = no_wait()
    client, server = await wire(lambda _: answer_reply("declare_other_agent"), retry=policy)

    with pytest.raises(VoraError) as excinfo:
        await client.pools.declare("owned", image="vora/base", cpu="100m", memory="1Mi")

    assert type(excinfo.value) is VoraError
    assert excinfo.value.status == 404
    assert len(server.requests) == 1
    assert waited == []


async def test_a_declare_refused_for_now_honours_retry_after_or_says_so(wire) -> None:
    policy, waited = no_wait()
    replies = iter(["declare_runner_down", "declare_lost_pool"])
    client, server = await wire(lambda _: answer_reply(next(replies)), retry=policy)

    pool = await client.pools.declare("py", image="vora/base", cpu="100m", memory="1Mi")

    assert pool.info.state == "POOL_ACTIVE"
    assert len(server.requests) == 2
    assert len(waited) == 1
    assert 1.0 <= waited[0] <= 1.0 + policy.base_delay

    client, _ = await wire(lambda _: answer_reply("declare_runner_down"), retry=None)
    with pytest.raises(UnavailableError) as excinfo:
        await client.pools.declare("py", image="vora/base", cpu="100m", memory="1Mi")
    assert excinfo.value.retry_after == 1.0


# --- What a later consumer relies on ---------------------------------------------


async def test_an_admin_declares_for_a_tenant_whose_key_then_runs_and_reads_each_frame(wire) -> None:
    runs = iter(["unsafe_no_reset", "warm_reset", "cold_first_run_nonzero_exit"])

    def route(sent: Sent) -> Reply:
        if sent.method == "PUT":
            return Reply(status=201, body=GOLDEN["answers"]["get_active"]["body"])
        if sent.method == "GET":
            return answer_reply("get_active")
        return run_reply(next(runs))

    admin, server = await wire(route)
    await admin.pools.declare(
        "py", image="vora/base", cpu="100m", memory="1Mi", tenant_id="tenant-a", reuse="unsafe_no_reset"
    )
    declared = json.loads(server.requests[0].body)
    assert (declared["tenantId"], declared["reuse"]) == ("tenant-a", "POOL_REUSE_UNSAFE_NO_RESET")

    async with Vora(base_url=server.url, api_key="vora_tenant") as tenant:
        pool = await tenant.pools.get("py")
        assert server.requests[-1].path == "/v1/pools/py"
        assert server.requests[-1].headers["authorization"] == "Bearer vora_tenant"

        seen = []
        for _ in range(3):
            run = exit_ = done = None
            async for chunk in pool.stream_run("true", [], timeout_seconds=10):
                if chunk.kind == "run":
                    run = chunk.run
                elif chunk.kind == "exit":
                    exit_ = chunk.exit
                elif chunk.kind == "run_done":
                    done = chunk.done
            assert run is not None and exit_ is not None and done is not None
            seen.append((run.warm, exit_.execution_id, exit_.exit_code, run.reset_ms, done.reset_ms, done.outcome))

    execution_id = "exe_01M6M3D3FRG49JM5BK3MD9Q0X0"
    assert seen == [
        (True, execution_id, 0, None, None, "POOL_RESET_SKIPPED"),
        (True, execution_id, 0, 7, 3, "POOL_RESET_OK"),
        (False, execution_id, 7, None, None, "POOL_RESET_RETIRED"),
    ]


@pytest.mark.parametrize("refusal", ["pool_full", "runner_loading"])
async def test_a_caller_can_handle_a_refusal_itself_with_no_policy(wire, refusal: str) -> None:
    policy, waited = no_wait()
    client, server = await wire(flaky(refusal, "warm_reset"), retry=policy)
    pool = handle(client)

    with pytest.raises((RateLimitedError, UnavailableError)):
        await pool.run("true", [], timeout_seconds=10)
    assert len(server.requests) == 1
    assert waited == []

    assert (await pool.run("true", [], timeout_seconds=10)).stdout == "42\n"


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


async def test_delete_of_a_pool_its_runner_lost_needs_one_call(wire) -> None:
    policy, waited = no_wait()
    client, server = await wire(lambda _: answer_reply("delete_not_on_runner"), retry=policy)

    await handle(client).delete()

    assert len(server.requests) == 1
    assert waited == []


async def test_delete_waits_out_a_missing_runner_row_and_then_says_so(wire) -> None:
    policy, waited = no_wait()
    client, server = await wire(lambda _: answer_reply("delete_runner_row_gone"), retry=policy)

    with pytest.raises(UnavailableError) as excinfo:
        await handle(client).delete(max_wait_seconds=0)

    assert excinfo.value.retry_after == 1.0
    assert len(server.requests) == 1

    replies = iter(["delete_runner_row_gone", "delete_runner_row_gone", "delete_gone"])
    client, server = await wire(lambda _: answer_reply(next(replies)), retry=policy)
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

    # The API bounds a stream at the run's timeout plus its tail, and a silent command
    # sends nothing to read until then.
    assert seen[0].extensions["timeout"]["read"] > 600 + LIMITS["runTailSeconds"]


async def test_a_short_run_still_waits_for_the_apis_504_on_its_first_frame() -> None:
    seen: list[httpx.Request] = []
    client = recording_client(seen, httpx.Response(429, json={"error": "full"}))

    with pytest.raises(RateLimitedError):
        await handle(client).run("true", timeout_seconds=1)

    # A client that gives up first turns the API's typed 504 into a bare read error.
    assert seen[0].extensions["timeout"]["read"] > LIMITS["runFirstFrameSeconds"]


async def test_a_declare_waits_for_the_apis_answer_when_it_loses_its_route() -> None:
    seen: list[httpx.Request] = []
    client = recording_client(seen, httpx.Response(201, json={"id": "pool_1"}))

    await client.pools.declare("py", image="vora/base", cpu="100m", memory="1Mi")

    assert seen[0].extensions["timeout"]["read"] > LIMITS["declareSeconds"]


async def test_no_first_frame_is_typed_and_a_run_does_not_retry_it(wire) -> None:
    policy, waited = no_wait()
    client, server = await wire(lambda _: run_reply("no_first_frame"))

    with pytest.raises(GatewayTimeoutError) as excinfo:
        await handle(client).run("true", [], timeout_seconds=10, retry=policy)

    assert excinfo.value.status == 504
    assert excinfo.value.retry_after == 1.0
    assert len(server.requests) == 1
    assert waited == []


async def test_a_silent_pool_run_is_not_cut_off_by_the_clients_read_timeout() -> None:
    server = WireServer(lambda _: run_reply("warm_reset", delay=0.3))
    await server.start()
    try:
        async with httpx.AsyncClient(timeout=0.05) as http:
            client = Vora(base_url=server.url, client=http)
            result = await handle(client).run("sleep 7", timeout_seconds=10)
    finally:
        await server.stop()

    assert result.stdout == "42\n"


async def test_plain_calls_outlast_the_apis_own_bound_whatever_the_clients_default() -> None:
    seen: list[httpx.Request] = []
    client = recording_client(seen, httpx.Response(200, json={"id": "pool_1"}))
    pool = handle(client)

    await client.pools.get("py")
    await client.pools.list()
    await pool.refresh()
    await pool.stats()
    await pool.execs("sbx_1")
    await pool.events("sbx_1")

    assert len(seen) == 6
    # httpx's default 5 s would turn the API's answer into a bare read error.
    assert all(request.extensions["timeout"]["read"] > LIMITS["callSeconds"] for request in seen)


# --- A plain exec's read timeout -------------------------------------------------


EXITED = '{"exit":{"exitCode":0,"reason":"EXITED"}}'


async def _exec(sandbox: Sandbox, **options: Any) -> Any:
    return await sandbox.exec("sleep 7", **options)


async def _stream_exec(sandbox: Sandbox, **options: Any) -> Any:
    return [chunk async for chunk in sandbox.stream_exec("sleep 7", **options)]


both_exec_calls = pytest.mark.parametrize("call", [_exec, _stream_exec], ids=["exec", "stream_exec"])
ExecCall = Callable[..., Awaitable[Any]]


def on(client: Vora) -> Sandbox:
    return Sandbox(client._transport, SandboxInfo(id="sbx_1"))


def recording_http(seen: list[httpx.Request], **options: Any) -> httpx.AsyncClient:
    def route(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return httpx.Response(200, text=EXITED + "\n")

    return httpx.AsyncClient(transport=httpx.MockTransport(route), **options)


def client_the_sdk_built(http: httpx.AsyncClient, base_url: str = "http://vora.test") -> Vora:
    client = Vora(base_url=base_url)
    client._transport._client = http
    return client


@both_exec_calls
@pytest.mark.parametrize("own_read_timeout", [7.0, 6000.0], ids=["client_shorter", "client_longer"])
async def test_an_execs_timeout_sets_its_read_timeout_over_a_callers_client(call: ExecCall, own_read_timeout: float) -> None:
    seen: list[httpx.Request] = []
    client = Vora(base_url="http://vora.test", client=recording_http(seen, timeout=own_read_timeout))

    await call(on(client), timeout_seconds=600)

    # The agent kills the group at 600 s, so its exit is due then. The margin is not a
    # second timeout: too short cuts off a run the agent is still ending.
    assert 600 + 10 <= seen[0].extensions["timeout"]["read"] <= 600 + 120


@both_exec_calls
async def test_an_execs_timeout_sets_its_read_timeout_on_a_client_the_sdk_built(call: ExecCall) -> None:
    seen: list[httpx.Request] = []
    client = client_the_sdk_built(recording_http(seen))

    await call(on(client), timeout_seconds=600)

    assert 600 + 10 <= seen[0].extensions["timeout"]["read"] <= 600 + 120


@both_exec_calls
@pytest.mark.parametrize("options", [{}, {"timeout_seconds": 0}], ids=["absent", "zero_means_no_limit_to_the_agent"])
async def test_with_no_exec_timeout_a_callers_client_keeps_its_own(call: ExecCall, options: dict[str, int]) -> None:
    seen: list[httpx.Request] = []
    client = Vora(base_url="http://vora.test", client=recording_http(seen, timeout=7.0))

    await call(on(client), **options)

    assert seen[0].extensions["timeout"]["read"] == 7.0


@both_exec_calls
@pytest.mark.parametrize("options", [{}, {"timeout_seconds": 0}], ids=["absent", "zero_means_no_limit_to_the_agent"])
async def test_with_no_exec_timeout_a_client_the_sdk_built_waits(call: ExecCall, options: dict[str, int]) -> None:
    seen: list[httpx.Request] = []
    client = client_the_sdk_built(recording_http(seen))

    await call(on(client), **options)

    assert seen[0].extensions["timeout"]["read"] is None


@both_exec_calls
async def test_silence_past_the_margin_raises_instead_of_hanging(call: ExecCall, monkeypatch) -> None:
    monkeypatch.setattr("vora._http._EXEC_READ_MARGIN_SECONDS", 0.2)
    # Headers, then nothing, as `exec("server &")` looks once its output is read.
    server = WireServer(lambda _: Reply(lines=[], hold=True))
    await server.start()
    async with httpx.AsyncClient(timeout=60.0) as http:
        client = Vora(base_url=server.url, client=http)
        with pytest.raises(httpx.ReadTimeout):
            await asyncio.wait_for(call(on(client), timeout_seconds=1), timeout=10)
    await server.stop()


@both_exec_calls
async def test_a_silent_exec_outlasts_a_callers_read_timeout_when_it_has_its_own(call: ExecCall) -> None:
    server = WireServer(lambda _: Reply(lines=[EXITED], delay=0.3))
    await server.start()
    async with httpx.AsyncClient(timeout=0.05) as http:
        client = Vora(base_url=server.url, client=http)
        await call(on(client), timeout_seconds=10)
    await server.stop()


@both_exec_calls
async def test_a_callers_read_timeout_ends_a_silent_exec_with_none_of_its_own(call: ExecCall) -> None:
    server = WireServer(lambda _: Reply(lines=[EXITED], delay=0.3))
    await server.start()
    async with httpx.AsyncClient(timeout=0.05) as http:
        client = Vora(base_url=server.url, client=http)
        with pytest.raises(httpx.ReadTimeout):
            await call(on(client))
    await server.stop()


@both_exec_calls
async def test_a_client_the_sdk_built_outlasts_its_default_on_a_silent_exec(call: ExecCall) -> None:
    server = WireServer(lambda _: Reply(lines=[EXITED], delay=0.3))
    await server.start()
    # An SDK-built client with a default shorter than the silence, to keep this test short.
    client = client_the_sdk_built(httpx.AsyncClient(timeout=0.05), server.url)

    await call(on(client))

    await client.aclose()
    await server.stop()


async def test_a_batch_client_with_its_own_timeout_keeps_working() -> None:
    # A caller that bounds its client past the exec's own timeout, and passes that too.
    server = WireServer(lambda _: Reply(lines=[EXITED], delay=0.3))
    await server.start()
    async with httpx.AsyncClient(timeout=httpx.Timeout(10 + 30.0)) as http:
        client = Vora(base_url=server.url, client=http)
        chunks = [chunk async for chunk in on(client).stream_exec("x", [], stdin="{}", timeout_seconds=10)]
    await server.stop()

    assert [chunk.kind for chunk in chunks] == ["exit"]


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

    pool = await client.pools.declare("py", image="vora/base", cpu="100m", memory="1Mi", tenant_id="tenant-a")
    await client.pools.get("py", tenant_id="tenant-a")
    await client.pools.list(tenant_id="tenant-a")
    await pool.stats()
    await pool.run("true", [], timeout_seconds=5)
    await pool.execs("sbx_1")
    await pool.events("sbx_1")
    await pool.delete()

    declare, *rest = server.requests
    assert json.loads(declare.body)["tenantId"] == "tenant-a"
    assert "tenantId" not in json.loads(declare.body)["sandbox"]
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
    fits = ((LIMITS["runBodyBytes"] - overhead) // 4) * 3

    await handle(client).run("cat", [], stdin=b"x" * fits, timeout_seconds=10)

    assert len(server.requests) == 1
    assert len(server.requests[0].body) <= LIMITS["runBodyBytes"]
    # The README's "about 768 KiB", which the envelope takes a little of.
    assert 767 * 1024 < fits < 768 * 1024

    with pytest.raises(RequestTooLargeError):
        await handle(client).run("cat", [], stdin=b"x" * (fits + 3), timeout_seconds=10)
    assert len(server.requests) == 1


async def test_a_body_of_exactly_the_apis_limit_is_sent_and_one_byte_more_is_not(wire) -> None:
    client, server = await wire(lambda _: run_reply("warm_reset"))
    limit = LIMITS["runBodyBytes"]
    envelope = len(json.dumps(exec_request("true", [], "", None, None, 10)))

    await handle(client).run("true", [], cwd="a" * (limit - envelope), timeout_seconds=10)

    assert len(server.requests[0].body) == limit
    with pytest.raises(RequestTooLargeError):
        await handle(client).run("true", [], cwd="a" * (limit - envelope + 1), timeout_seconds=10)
    assert len(server.requests) == 1


# --- Listings --------------------------------------------------------------------


async def test_a_pool_whose_runner_did_not_answer_has_no_state_and_no_counters(wire) -> None:
    client, _ = await wire(lambda _: answer_reply("listing_runner_silent"))

    pools = await client.pools.list()

    assert len(pools) == 1
    assert pools[0].name == "py"
    assert pools[0].info.state is None
    assert pools[0].info.stats is None


async def test_a_lost_pool_stays_listed_with_nothing_measured(wire) -> None:
    client, _ = await wire(lambda _: answer_reply("listing_not_on_runner"))

    pools = await client.pools.list()

    assert [pool.name for pool in pools] == ["py"]
    assert pools[0].info.state == "POOL_NOT_ON_RUNNER"
    assert pools[0].info.stats is None


async def test_an_unanswered_listing_entry_still_says_whether_it_resets(wire) -> None:
    client, _ = await wire(lambda _: answer_reply("listing_unsafe_unanswered"))

    by_name = {pool.name: pool.info for pool in await client.pools.list()}

    unsafe = by_name["unsafe"]
    assert unsafe.state is None
    assert unsafe.stats is None
    assert unsafe.spec is not None
    assert unsafe.spec.reuse == "POOL_REUSE_UNSAFE_NO_RESET"
    assert by_name["py"].spec is not None
    assert by_name["py"].spec.reuse is None


async def test_a_handle_from_a_listing_of_every_tenant_reaches_its_own_tenant(wire) -> None:
    client, server = await wire(
        lambda sent: answer_reply("listing_runner_silent") if sent.path == "/v1/pools" else answer_reply("get_active")
    )

    pool = (await client.pools.list())[0]
    await pool.refresh()

    assert server.requests[-1].path == "/v1/pools/py?tenant_id=tenant-a"


async def test_a_not_loaded_pool_has_no_stats_though_the_wire_sends_an_empty_one(wire) -> None:
    client, _ = await wire(lambda _: answer_reply("get_not_loaded"))
    assert json.loads(GOLDEN["answers"]["get_not_loaded"]["body"])["stats"] == {}

    pool = await client.pools.get("py")

    assert pool.info.state == "POOL_NOT_LOADED"
    assert pool.info.not_loaded_reason == "backend_missing"
    assert pool.info.stats is None
    assert await pool.stats() is None


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


def test_an_audit_record_and_a_sandbox_name_their_pool() -> None:
    record = decode_sandbox_exec({"poolId": "pool_1", "sandboxRun": 2})
    assert (record.pool_id, record.sandbox_run) == ("pool_1", 2)
    assert decode_sandbox_info({"id": "sbx_1", "poolId": "pool_1"}).pool_id == "pool_1"
    assert decode_sandbox_info({"id": "sbx_1"}).pool_id is None


def test_a_retry_after_date_from_a_proxy_is_understood() -> None:
    from vora._http import _parse_retry_after

    assert _parse_retry_after("2") == 2.0
    assert _parse_retry_after("Wed, 21 Oct 2015 07:28:00 GMT") == 0.0
    assert _parse_retry_after("soon") is None
    assert _parse_retry_after("nan") is None
    assert _parse_retry_after(None) is None


def test_the_public_surface_is_exported() -> None:
    for name in ("Pool", "Pools", "RetryPolicy", "RunChunk", "PoolRunResult", "PoolRunError", "PoolStats"):
        assert name in vora.__all__
        assert hasattr(vora, name)


