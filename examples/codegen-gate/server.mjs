// codegen-gate: POST /review { code, tests } -> run generated code against its
// tests in a sandbox, return a verdict. Dependency-free on purpose: the trust
// boundary must not rely on the codegen stack's own dependencies.
import { createServer } from "node:http";

import { Vora } from "@vorash/sdk";

const vora = new Vora(); // VORA_API_URL + VORA_API_KEY from the environment

const server = createServer((req, res) => {
  const chunks = [];
  req.on("data", (chunk) => chunks.push(chunk));
  req.on("end", async () => {
    if (req.method !== "POST" || req.url !== "/review") {
      res.writeHead(404).end();
      return;
    }
    let verdict;
    try {
      verdict = await review(JSON.parse(Buffer.concat(chunks).toString("utf8")));
      res.writeHead(200, { "Content-Type": "application/json" });
    } catch (error) {
      verdict = { verdict: "error", message: String(error) };
      res.writeHead(400, { "Content-Type": "application/json" });
    }
    res.end(JSON.stringify(verdict));
  });
});

async function review({ code, tests }) {
  if (typeof code !== "string" || typeof tests !== "string") {
    throw new Error("body must be { code: string, tests: string }");
  }
  // The workload gets a fresh sandbox per review: no network, tiny budgets,
  // 30s of wall clock. Hostile or broken code dies at the boundary.
  const sandbox = await vora.sandboxes.create({
    image: "vora/python",
    backend: process.env.VORA_BACKEND ?? "bwrap",
    cpu: 0.5,
    memory: "128Mi",
    pids: 32,
    ttl: "2m",
    network: "none",
  });
  try {
    await sandbox.writeFile("/workspace/solution.py", code);
    await sandbox.writeFile("/workspace/test_solution.py", tests);
    let result;
    try {
      result = await sandbox.exec({
        // Plain assert-script as the test runner: the image ships no pytest and
        // the sandbox has no network to install one.
        command: "python3",
        args: ["test_solution.py"],
        cwd: "/workspace",
        timeoutSeconds: 30,
      });
    } catch (error) {
      // A killed run (timeout, OOM, runaway fork) closes the stream without an
      // exit record — the SDK refuses to call that success, and so do we.
      const partial = error.body ?? {};
      return {
        verdict: "killed",
        reason: String(error.message),
        output: `${partial.stdout ?? ""}${partial.stderr ?? ""}`.slice(-2000),
      };
    }
    const failed = result.exitCode !== 0;
    const killed = result.reason !== undefined && result.reason !== "EXITED";
    return {
      verdict: killed ? "killed" : failed ? "failed" : "accepted",
      exitCode: result.exitCode,
      reason: result.reason ?? "EXITED",
      output: (result.stdout + result.stderr).slice(-2000),
    };
  } finally {
    await sandbox.destroy(); // review is ephemeral: verdict out, sandbox gone
  }
}

const port = Number(process.env.PORT ?? 9400);
server.listen(port, "127.0.0.1", () => {
  console.log(`codegen-gate listening on http://127.0.0.1:${port}/review`);
});
