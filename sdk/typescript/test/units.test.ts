import { describe, expect, it } from "vitest";

import { VoraError, parseCPU, parseDuration, parseSize } from "../src/index.js";

describe("parseSize", () => {
  it("accepts byte counts and binary suffixes", () => {
    expect(parseSize(1024)).toBe(1024);
    expect(parseSize("512Mi")).toBe(512 * 1024 ** 2);
    expect(parseSize("1Gi")).toBe(1024 ** 3);
    expect(parseSize("2 Mi")).toBe(2 * 1024 ** 2);
  });

  it("treats bare K/M/G as decimal", () => {
    expect(parseSize("500M")).toBe(500 * 1000 ** 2);
    expect(parseSize("2G")).toBe(2 * 1000 ** 3);
  });

  it("rejects nonsense", () => {
    expect(() => parseSize("lots")).toThrow(VoraError);
    expect(() => parseSize("512Xi")).toThrow(/unknown size unit/);
    expect(() => parseSize(-1)).toThrow(VoraError);
  });
});

describe("parseCPU", () => {
  it("reads cores and millicores", () => {
    expect(parseCPU(1)).toBe(1000);
    expect(parseCPU(0.5)).toBe(500);
    expect(parseCPU("250m")).toBe(250);
    expect(parseCPU("2")).toBe(2000);
  });

  it("rejects nonsense", () => {
    expect(() => parseCPU("fast")).toThrow(VoraError);
  });
});

describe("parseDuration", () => {
  it("reads seconds and compound durations", () => {
    expect(parseDuration(90)).toBe(90);
    expect(parseDuration("90")).toBe(90);
    expect(parseDuration("30s")).toBe(30);
    expect(parseDuration("5m")).toBe(300);
    expect(parseDuration("1h30m")).toBe(5400);
    expect(parseDuration("2h")).toBe(7200);
  });

  it("rejects trailing junk", () => {
    expect(() => parseDuration("1h30x")).toThrow(VoraError);
    expect(() => parseDuration("soon")).toThrow(VoraError);
  });
});