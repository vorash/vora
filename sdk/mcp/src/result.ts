import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";

/** A successful tool call: human-readable text plus optional structured fields. */
export function ok(text: string, structured?: Record<string, unknown>): CallToolResult {
  const result: CallToolResult = { content: [{ type: "text", text }] };
  if (structured !== undefined) {
    result.structuredContent = structured;
  }
  return result;
}

/**
 * A failed tool call. Errors are returned rather than thrown so the agent sees
 * the message and can react, instead of the host surfacing a transport error.
 */
export function fail(error: unknown): CallToolResult {
  return { content: [{ type: "text", text: describe(error) }], isError: true };
}

function describe(error: unknown): string {
  if (error instanceof Error) {
    return error.message;
  }
  return String(error);
}