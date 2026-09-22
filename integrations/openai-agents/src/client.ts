import { tool } from "@openai/agents";
import type { FunctionTool, UnknownContext } from "@openai/agents";
import { Vora, VoraError } from "@vorash/sdk";
import type {
  CPUInput,
  DurationInput,
  ExitReason,
  NetworkOption,
  Sandbox,
  SizeInput,
} from "@vorash/sdk";
import { z } from "zod";

/** Code flavours `run_code` can execute, and the interpreter each one uses. */
export type RunCodeLanguage = "python" | "javascript" | "shell";

const INTERPRETERS: Record<RunCodeLanguage, string> = {
  python: "python3",
  javascript: "node",
  shell: "sh",
};

export interface RunCodeArgs {
  code: string;
  /** Defaults to `python`. The image must contain the matching interpreter. */
  language?: RunCodeLanguage;
  timeoutSeconds?: number;
}

export interface RunCodeResult {
  stdout: string;
  stderr: string;
  exitCode: number;
  reason?: ExitReason;
  /** What the model sees: exit status plus the captured streams. */
  output: string;
}

export interface VoraSandboxClientOptions {
  /** Pre-built client. Defaults to `new Vora()`, which reads the VORA_* env vars. */
  client?: Vora;
  /** Rootfs image. Defaults to `vora/python`, matching the default language. */
  image?: string;
  memory?: SizeInput;
  cpu?: CPUInput;
  pids?: number;
  network?: NetworkOption;
  env?: string[];
  ttl?: DurationInput;
  backend?: string;
  /** Working directory for `run_code`. Defaults to `/workspace`. */
  workdir?: string;
  /**
   * Run against an existing sandbox instead of creating one. The client does
   * not own an adopted sandbox, so `dispose()` leaves it running.
   */
  sandboxId?: string;
}

export const RUN_CODE_TOOL_NAME = "run_code";

const RUN_CODE_DESCRIPTION =
  "Execute code in an isolated Vora sandbox and return its stdout, stderr and exit code. " +
  "The sandbox is a throwaway Linux environment, so it is safe to run untrusted code. " +
  "Use it to compute, test snippets or inspect files; state persists between calls.";

const runCodeParameters = z.object({
  code: z.string().describe("The program to execute, read from standard input"),
  language: z
    .enum(["python", "javascript", "shell"])
    .optional()
    .describe("Language to run; defaults to python"),
  timeout_seconds: z.number().int().positive().optional().describe("Wall-clock limit for the program"),
});

export type RunCodeTool = FunctionTool<UnknownContext, typeof runCodeParameters, string>;

/**
 * Runs code for an OpenAI Agent inside a Vora sandbox.
 *
 * The adapter owns one sandbox for its lifetime and reuses it across calls, so
 * files and installed packages persist between `run_code` invocations.
 */
export class VoraSandboxClient {
  private readonly vora: Vora;
  private readonly options: VoraSandboxClientOptions;

  private current: Sandbox | undefined;
  private starting: Promise<Sandbox> | undefined;
  private ownsSandbox = false;

  constructor(options: VoraSandboxClientOptions = {}) {
    this.options = options;
    this.vora = options.client ?? new Vora();
  }

  /** The live sandbox, once `start()` (or the first `runCode`) has provisioned it. */
  get sandbox(): Sandbox | undefined {
    return this.current;
  }

  /** Provision the sandbox if needed. Concurrent callers share one creation. */
  async start(): Promise<Sandbox> {
    if (this.current !== undefined) {
      return this.current;
    }
    this.starting ??= this.provision();
    try {
      this.current = await this.starting;
    } catch (error) {
      this.starting = undefined;
      throw error;
    }
    return this.current;
  }

  /** Execute `code` in the sandbox and collect its output. */
  async runCode(args: RunCodeArgs): Promise<RunCodeResult> {
    const language = args.language ?? "python";
    const interpreter = INTERPRETERS[language];
    if (interpreter === undefined) {
      throw new VoraError(`unsupported language: ${String(language)}`);
    }
    const sandbox = await this.start();
    const result = await sandbox.exec({
      command: interpreter,
      stdin: args.code,
      cwd: this.options.workdir ?? "/workspace",
      timeoutSeconds: args.timeoutSeconds,
    });
    const output: RunCodeResult = {
      stdout: result.stdout,
      stderr: result.stderr,
      exitCode: result.exitCode,
      output: render(result.stdout, result.stderr, result.exitCode, result.reason),
    };
    if (result.reason !== undefined) {
      output.reason = result.reason;
    }
    return output;
  }

  /**
   * The OpenAI Agents tool. It returns a readable result on failure rather than
   * throwing, so the model can see what went wrong and try again.
   */
  tool(): RunCodeTool {
    return tool({
      name: RUN_CODE_TOOL_NAME,
      description: RUN_CODE_DESCRIPTION,
      parameters: runCodeParameters,
      execute: async (args) => {
        try {
          const result = await this.runCode({
            code: args.code,
            language: args.language,
            timeoutSeconds: args.timeout_seconds,
          });
          return result.output;
        } catch (error) {
          return `error: ${error instanceof Error ? error.message : String(error)}`;
        }
      },
    });
  }

  /** Destroy the sandbox, if this client created it. Safe to call twice. */
  async dispose(): Promise<void> {
    const sandbox = this.current;
    this.current = undefined;
    this.starting = undefined;
    if (sandbox === undefined || !this.ownsSandbox) {
      return;
    }
    this.ownsSandbox = false;
    await sandbox.destroy();
  }

  private async provision(): Promise<Sandbox> {
    const { sandboxId } = this.options;
    if (sandboxId !== undefined) {
      // Adopted sandboxes are someone else's to destroy.
      return this.vora.sandboxes.get(sandboxId);
    }
    const sandbox = await this.vora.sandboxes.create({
      image: this.options.image ?? "vora/python",
      memory: this.options.memory,
      cpu: this.options.cpu,
      pids: this.options.pids,
      network: this.options.network,
      env: this.options.env,
      ttl: this.options.ttl,
      backend: this.options.backend,
    });
    this.ownsSandbox = true;
    return sandbox;
  }
}

function render(stdout: string, stderr: string, exitCode: number, reason?: ExitReason): string {
  let text = reason === undefined ? `exit ${exitCode}` : `exit ${exitCode} (${reason})`;
  if (stdout !== "") {
    text += `\n\nstdout:\n${stdout}`;
  }
  if (stderr !== "") {
    text += `\n\nstderr:\n${stderr}`;
  }
  return text;
}