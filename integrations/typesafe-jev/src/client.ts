import { tool } from "@openai/agents";
import type { FunctionTool, UnknownContext } from "@openai/agents";
import { TypeSafeClient, TypeSafeError } from "@typesafe-ai/sdk";
import type {
  EntryType,
  Fetch,
  Questions,
  SystemOneResult,
} from "@typesafe-ai/sdk";

import {
  CONFIDENCE_NOTE,
  JEV_DECIDE_TOOL_NAME,
  STATE_NOTE,
  decideJsonSchema,
  parseDecideArgs,
} from "./schema.js";

const DEFAULT_MODEL = "jev-latest";

export interface JevDecisionClientOptions {
  /** Pre-built SDK client; when given, the connection options below are ignored. */
  client?: TypeSafeClient;
  /** TypeSafe API key. Defaults to the SDK's own `TYPESAFE_API_KEY` resolution. */
  apiKey?: string;
  /** API root. Defaults to `https://api.typesafe.ai`. */
  baseUrl?: string;
  /** Model to request. Defaults to `jev-latest`; pin one (e.g. `jev-1.13.0`) for reproducibility. */
  model?: string;
  /** Timeout per attempt in milliseconds. */
  timeoutMs?: number;
  /** Maximum retries after the initial attempt. */
  maxRetries?: number;
  /** Custom fetch implementation, for tests or transport configuration. */
  fetch?: Fetch;
}

export interface DecisionResult<Q extends Questions> extends SystemOneResult<Q> {
  /** The model we asked for; `model` is what the provider actually answered with. */
  requestedModel: string;
}

const DECIDE_DESCRIPTION =
  "Ask Jev, a TypeSafe decision model, narrow structured questions about one piece of context " +
  "(the state). Send every question in one call. Three question types: `choice` picks one " +
  "label from `criteria`, `score` rates against an ordered `criteria` rubric, `noul` returns " +
  "the probability of yes. Each answer comes with probabilities and a `confidence`. " +
  CONFIDENCE_NOTE +
  " " +
  STATE_NOTE;

export type JevDecideTool = FunctionTool<UnknownContext, typeof decideJsonSchema, string>;

/**
 * A thin wrapper over TypeSafe's JEV decision model.
 *
 * `decide` is the programmatic path (typed answers, throws on failure); `tool`
 * is the model-facing path (one `jev_decide` tool, `error:` strings on failure).
 */
export class JevDecisionClient {
  private readonly client: TypeSafeClient;
  private readonly model: string;

  constructor(options: JevDecisionClientOptions = {}) {
    this.model = options.model ?? DEFAULT_MODEL;
    this.client =
      options.client ??
      new TypeSafeClient({
        apiKey: options.apiKey,
        baseURL: options.baseUrl,
        defaultModel: this.model,
        timeout: options.timeoutMs,
        retry:
          options.maxRetries === undefined ? undefined : { maxRetries: options.maxRetries },
        fetch: options.fetch,
      });
  }

  /** Answer every question about `state` in one round trip. */
  async decide<Q extends Questions>(state: EntryType, questions: Q): Promise<DecisionResult<Q>> {
    const result = await this.client.systemOne({ state, questions, model: this.model });
    assertComplete(questions, result.answers);
    return { ...result, requestedModel: this.model };
  }

  /**
   * The OpenAI Agents tool. It returns a readable result on failure rather than
   * throwing, so the model can see what went wrong and try again.
   *
   * Non-strict, because the questions and criteria are free-form maps that
   * strict Structured Outputs cannot express; arguments are validated against
   * `decideParameters` inside `execute` instead.
   */
  tool(): JevDecideTool {
    return tool({
      name: JEV_DECIDE_TOOL_NAME,
      description: DECIDE_DESCRIPTION,
      parameters: decideJsonSchema,
      strict: false,
      execute: async (args) => {
        try {
          const { state, questions } = parseDecideArgs(args);
          // The zod shapes mirror the SDK's question types; the score rubric is a
          // tuple there and an array here, so the model-facing edge casts once.
          const result = await this.decide(state, questions as Questions);
          return JSON.stringify(result);
        } catch (error) {
          return `error: ${error instanceof Error ? error.message : String(error)}`;
        }
      },
    });
  }
}

/**
 * A response that drops or invents answers is a protocol failure, not a sparse
 * result. The error names every offending question so the failure is diagnosable.
 */
function assertComplete(questions: Questions, answers: SystemOneResult<Questions>["answers"]): void {
  const missing: string[] = [];
  const mismatched: string[] = [];
  for (const [name, question] of Object.entries(questions)) {
    const answer = answers[name];
    if (answer === undefined) {
      missing.push(name);
      continue;
    }
    if (answer.type !== question.type) {
      mismatched.push(`${name} answered ${answer.type}, asked ${question.type}`);
    }
  }
  const extra = Object.keys(answers)
    .filter((name) => !(name in questions))
    .sort();
  missing.sort();

  const problems: string[] = [];
  if (missing.length > 0) {
    problems.push("response has no answer for " + missing.join(", "));
  }
  if (extra.length > 0) {
    problems.push("response contains answers that were not asked for: " + extra.join(", "));
  }
  problems.push(...mismatched);
  if (problems.length > 0) {
    throw new TypeSafeError(problems.join("; "));
  }
}
