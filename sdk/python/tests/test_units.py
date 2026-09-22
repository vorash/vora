"""Unit parsing parity with sdk/typescript/src/units.ts."""

from __future__ import annotations

import pytest

from vora import VoraError, parse_cpu, parse_duration, parse_size


def test_parse_size() -> None:
    assert parse_size("512Mi") == 512 * 1024**2
    assert parse_size("1Gi") == 1024**3
    assert parse_size("500M") == 500 * 1000**2
    assert parse_size("1024") == 1024
    assert parse_size(4096) == 4096


def test_parse_cpu() -> None:
    assert parse_cpu(1) == 1000
    assert parse_cpu("500m") == 500
    assert parse_cpu("2") == 2000


def test_parse_duration() -> None:
    assert parse_duration("90s") == 90
    assert parse_duration("30m") == 1800
    assert parse_duration("1h30m") == 5400
    assert parse_duration("30") == 30
    assert parse_duration(45) == 45


@pytest.mark.parametrize("bad", ["", "abc", "12x"])
def test_parse_duration_rejects_garbage(bad: str) -> None:
    with pytest.raises(VoraError):
        parse_duration(bad)


def test_parse_size_rejects_unknown_units() -> None:
    with pytest.raises(VoraError):
        parse_size("512Xi")
