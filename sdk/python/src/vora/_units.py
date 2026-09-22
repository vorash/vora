"""Parse human-friendly resource strings the way container runtimes quote them.

Mirrors ``sdk/typescript/src/units.ts``: binary suffixes (Ki/Mi/Gi) are powers
of 1024, decimal ones (K/M/G) are powers of 1000.
"""

from __future__ import annotations

import re
from typing import TypeAlias

from ._errors import VoraError

SizeInput: TypeAlias = int | float | str
CPUInput: TypeAlias = int | float | str
DurationInput: TypeAlias = int | float | str

_SIZE_UNITS: dict[str, int] = {
    "": 1,
    "b": 1,
    "k": 1000,
    "kb": 1000,
    "m": 1000**2,
    "mb": 1000**2,
    "g": 1000**3,
    "gb": 1000**3,
    "t": 1000**4,
    "tb": 1000**4,
    "ki": 1024,
    "kib": 1024,
    "mi": 1024**2,
    "mib": 1024**2,
    "gi": 1024**3,
    "gib": 1024**3,
    "ti": 1024**4,
    "tib": 1024**4,
}

_SIZE_RE = re.compile(r"^\s*(\d+(?:\.\d+)?)\s*([a-z]*)\s*$", re.IGNORECASE)
_CPU_RE = re.compile(r"^\s*(\d+(?:\.\d+)?)(m?)\s*$", re.IGNORECASE)
_DURATION_PART_RE = re.compile(r"(\d+(?:\.\d+)?)\s*([hms])", re.IGNORECASE)
_DURATION_SCALES = {"h": 3600, "m": 60, "s": 1}


def _checked(value: float, kind: str) -> int:
    if value < 0:
        raise VoraError(f"invalid {kind}: {value}")
    return int(value)


def parse_size(value: int | float | str) -> int:
    """Parse ``"512Mi"``, ``"1Gi"``, ``"500M"`` or a byte count into bytes."""
    if isinstance(value, (int, float)):
        return _checked(value, "size")
    match = _SIZE_RE.match(value)
    if match is None:
        raise VoraError(f"invalid size: {value!r}")
    multiplier = _SIZE_UNITS.get(match.group(2).lower())
    if multiplier is None:
        raise VoraError(f"unknown size unit: {match.group(2)!r}")
    return _checked(float(match.group(1)) * multiplier, "size")


def parse_cpu(value: int | float | str) -> int:
    """Parse cores (``1``, ``0.5``) or millicores (``"500m"``) into millicores."""
    if isinstance(value, (int, float)):
        return _checked(value * 1000, "cpu")
    match = _CPU_RE.match(value)
    if match is None:
        raise VoraError(f"invalid cpu: {value!r}")
    amount = float(match.group(1))
    return _checked(amount if match.group(2).lower() == "m" else amount * 1000, "cpu")


def parse_duration(value: int | float | str) -> int:
    """Parse a duration into seconds: a number, ``"90s"``, ``"30m"``, ``"1h30m"``."""
    if isinstance(value, (int, float)):
        return _checked(value, "duration")
    trimmed = value.strip()
    if re.match(r"^\d+(?:\.\d+)?$", trimmed):
        return _checked(float(trimmed), "duration")
    total = 0.0
    consumed = 0
    for part in _DURATION_PART_RE.finditer(trimmed):
        total += float(part.group(1)) * _DURATION_SCALES[part.group(2).lower()]
        consumed += len(part.group(0))
    nonspace = len("".join(trimmed.split()))
    if consumed == 0 or consumed != nonspace:
        raise VoraError(f"invalid duration: {value!r}")
    return _checked(total, "duration")
