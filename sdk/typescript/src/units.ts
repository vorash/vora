import { VoraError } from "./errors.js";

import type { CPUInput, DurationInput, SizeInput } from "./types.js";

// Memory and disk sizes accept a plain byte count or a suffixed string. Binary
// suffixes (Ki/Mi/Gi) are powers of 1024; decimal ones (K/M/G) are powers of
// 1000, matching the way container runtimes quote them.
const SIZE_UNITS: Record<string, number> = {
  "": 1,
  b: 1,
  k: 1000,
  kb: 1000,
  m: 1000 ** 2,
  mb: 1000 ** 2,
  g: 1000 ** 3,
  gb: 1000 ** 3,
  t: 1000 ** 4,
  tb: 1000 ** 4,
  ki: 1024,
  kib: 1024,
  mi: 1024 ** 2,
  mib: 1024 ** 2,
  gi: 1024 ** 3,
  gib: 1024 ** 3,
  ti: 1024 ** 4,
  tib: 1024 ** 4,
};

/** Parse `"512Mi"`, `"1Gi"`, `"500M"` or a byte count into bytes. */
export function parseSize(value: SizeInput): number {
  if (typeof value === "number") {
    return checked(value, "size");
  }
  const match = /^\s*(\d+(?:\.\d+)?)\s*([a-z]*)\s*$/i.exec(value);
  if (match === null) {
    throw new VoraError(`invalid size: ${JSON.stringify(value)}`);
  }
  const unit = (match[2] ?? "").toLowerCase();
  const multiplier = SIZE_UNITS[unit];
  if (multiplier === undefined) {
    throw new VoraError(`unknown size unit: ${JSON.stringify(match[2])}`);
  }
  return checked(Number(match[1]) * multiplier, "size");
}

/** Parse CPU cores (`1`, `0.5`) or millicores (`"500m"`) into millicores. */
export function parseCPU(value: CPUInput): number {
  if (typeof value === "number") {
    return checked(value * 1000, "cpu");
  }
  const match = /^\s*(\d+(?:\.\d+)?)(m?)\s*$/i.exec(value);
  if (match === null) {
    throw new VoraError(`invalid cpu: ${JSON.stringify(value)}`);
  }
  const amount = Number(match[1]);
  return checked((match[2] ?? "").toLowerCase() === "m" ? amount : amount * 1000, "cpu");
}

/** Parse a duration into seconds: a number, `"90s"`, `"30m"`, `"1h30m"`. */
export function parseDuration(value: DurationInput): number {
  if (typeof value === "number") {
    return checked(value, "duration");
  }
  const trimmed = value.trim();
  if (/^\d+(?:\.\d+)?$/.test(trimmed)) {
    return checked(Number(trimmed), "duration");
  }
  const parts = trimmed.matchAll(/(\d+(?:\.\d+)?)\s*([hms])/gi);
  let total = 0;
  let consumed = 0;
  const scale: Record<string, number> = { h: 3600, m: 60, s: 1 };
  for (const part of parts) {
    const factor = scale[(part[2] ?? "").toLowerCase()];
    if (factor === undefined) {
      throw new VoraError(`invalid duration: ${JSON.stringify(value)}`);
    }
    total += Number(part[1]) * factor;
    consumed += part[0].length;
  }
  if (consumed === 0 || consumed !== trimmed.replace(/\s+/g, "").length) {
    throw new VoraError(`invalid duration: ${JSON.stringify(value)}`);
  }
  return checked(total, "duration");
}

function checked(value: number, what: string): number {
  if (!Number.isFinite(value) || value < 0) {
    throw new VoraError(`invalid ${what}: ${value}`);
  }
  return Math.round(value);
}