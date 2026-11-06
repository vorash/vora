"""Pools against a real vora-api, which needs a host that can run sandboxes.

Skipped unless VORA_API_URL and VORA_API_KEY are set. VORA_E2E_IMAGE and
VORA_E2E_BACKEND choose the pool's image and backend. VORA_E2E_UNSAFE=1 also runs
the unsafe pool, which needs an admin key and a runner that grants it.
"""

from __future__ import annotations

import os
import uuid

import pytest

from vora import PoolSpecConflictError, Vora

pytestmark = pytest.mark.skipif(
    not (os.environ.get("VORA_API_URL") and os.environ.get("VORA_API_KEY")),
    reason="needs VORA_API_URL and VORA_API_KEY",
)

IMAGE = os.environ.get("VORA_E2E_IMAGE", "vora/base")
BACKEND = os.environ.get("VORA_E2E_BACKEND", "bwrap")


def pool_name() -> str:
    return f"sdk-e2e-{uuid.uuid4().hex[:12]}"


async def test_a_pool_reuses_a_sandbox_and_proves_the_reset() -> None:
    async with Vora() as client:
        name = pool_name()
        pool = await client.pools.declare(
            name, image=IMAGE, backend=BACKEND, cpu="500m", memory="256Mi", pids=64, size=1, max_size=1
        )
        try:
            first = await pool.run("sh", ["-c", "echo first > /tmp/left; echo out"], timeout_seconds=30)
            assert first.stdout == "out\n"
            assert first.exit.exit_code == 0
            assert first.exit.reason == "EXITED"
            assert first.exit.usage is not None
            assert first.done is not None and first.done.outcome == "POOL_RESET_OK"

            second = await pool.run("sh", ["-c", "cat /tmp/left || echo clean"], timeout_seconds=30)
            assert second.run.sandbox_id == first.run.sandbox_id
            assert second.run.sandbox_run == 2
            assert "clean" in second.stdout
            assert second.run.reset_ms is not None

            execs = await pool.execs(first.run.sandbox_id or "")
            assert [record.sandbox_run for record in execs] == [1, 2]

            same = await client.pools.declare(
                name, image=IMAGE, backend=BACKEND, cpu="500m", memory="256Mi", pids=64, size=1, max_size=1
            )
            assert same.id == pool.id
            with pytest.raises(PoolSpecConflictError):
                await client.pools.declare(
                    name, image=IMAGE, backend=BACKEND, cpu="500m", memory="256Mi", pids=64, size=1, max_size=2
                )
        finally:
            await pool.delete()


async def test_a_cold_pool_starts_every_run_cold() -> None:
    async with Vora() as client:
        pool = await client.pools.declare(
            pool_name(), image=IMAGE, backend=BACKEND, cpu="500m", memory="256Mi", pids=64, size=0, max_size=1
        )
        try:
            result = await pool.run("true", [], timeout_seconds=30)
            assert result.run.warm is False
            assert result.run.startup_ms is not None and result.run.startup_ms > 0
            assert result.exit.exit_code == 0
        finally:
            await pool.delete()


@pytest.mark.skipif(not os.environ.get("VORA_E2E_UNSAFE"), reason="needs VORA_E2E_UNSAFE=1")
async def test_an_unsafe_pool_shows_in_its_results() -> None:
    async with Vora() as client:
        pool = await client.pools.declare(
            pool_name(),
            image=IMAGE,
            backend=BACKEND,
            cpu="500m",
            memory="256Mi",
            pids=64,
            size=1,
            max_size=1,
            reuse="unsafe_no_reset",
        )
        try:
            result = await pool.run("true", [], timeout_seconds=30)
            assert result.run.reuse == "POOL_REUSE_UNSAFE_NO_RESET"
            assert result.done is not None and result.done.outcome == "POOL_RESET_SKIPPED"
        finally:
            await pool.delete()
