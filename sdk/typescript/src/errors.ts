/** A failed Vora API call, or a malformed request built by this SDK. */
export class VoraError extends Error {
  /** HTTP status, when the failure came from the API. */
  readonly status?: number;
  /** Parsed response body (`{"error": "..."}`), when the API returned one. */
  readonly body?: unknown;

  constructor(message: string, options: { status?: number; body?: unknown } = {}) {
    super(message);
    this.name = "VoraError";
    this.status = options.status;
    this.body = options.body;
  }
}