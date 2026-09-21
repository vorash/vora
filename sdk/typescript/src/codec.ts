import { VoraError } from "./errors.js";

const buffer = (
  globalThis as {
    Buffer?: {
      from(input: Uint8Array): { toString(encoding: string): string };
      from(input: string, encoding: string): Uint8Array;
    };
  }
).Buffer;

const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();

/** Decode a protojson `bytes` field (base64) into raw bytes. */
export function decodeBase64(value: string): Uint8Array {
  if (buffer !== undefined) {
    return new Uint8Array(buffer.from(value, "base64"));
  }
  const binary = atob(value);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) {
    out[i] = binary.charCodeAt(i);
  }
  return out;
}

/** Encode raw bytes or a UTF-8 string for a protojson `bytes` field. */
export function encodeBase64(value: string | Uint8Array): string {
  const bytes = typeof value === "string" ? textEncoder.encode(value) : value;
  if (buffer !== undefined) {
    return buffer.from(bytes).toString("base64");
  }
  let binary = "";
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  return btoa(binary);
}

/** Decode a UTF-8 byte sequence. */
export function decodeText(value: Uint8Array): string {
  return textDecoder.decode(value);
}

/**
 * Yield one parsed JSON value per newline-delimited line. The exec endpoint
 * streams `v1.ExecChunk` as NDJSON, and the final line may arrive without a
 * trailing newline, so the tail is flushed on close.
 */
export async function* ndjson<T>(body: ReadableStream<Uint8Array>): AsyncGenerator<T> {
  const reader = body.getReader();
  let pending = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }
      pending += textDecoder.decode(value, { stream: true });
      let newline = pending.indexOf("\n");
      while (newline !== -1) {
        const line = pending.slice(0, newline).trim();
        pending = pending.slice(newline + 1);
        if (line !== "") {
          yield parseLine<T>(line);
        }
        newline = pending.indexOf("\n");
      }
    }
    pending += textDecoder.decode();
    const tail = pending.trim();
    if (tail !== "") {
      yield parseLine<T>(tail);
    }
  } finally {
    reader.releaseLock();
  }
}

function parseLine<T>(line: string): T {
  try {
    return JSON.parse(line) as T;
  } catch {
    throw new VoraError(`malformed stream line: ${line.slice(0, 200)}`);
  }
}