// data-workbench: run an untrusted analysis script over a shared dataset in a
// sandbox — no network, bounded CPU/memory, hard disk quota — and return the
// outputs as data.
//
//   node data-workbench/workbench.mjs <analysis.py> [data.csv]
import { readFile } from "node:fs/promises";
import process from "node:process";

import { Vora } from "@vorash/sdk";

const [, , scriptPath, dataPath] = process.argv;
if (scriptPath === undefined) {
  console.error("usage: node workbench.mjs <analysis-script.py> [data.csv]");
  process.exit(2);
}

const vora = new Vora();
const script = await readFile(scriptPath, "utf8");
const data = dataPath === undefined ? "" : await readFile(dataPath, "utf8");

const sandbox = await vora.sandboxes.create({
  image: "vora/python",
  backend: process.env.VORA_BACKEND ?? "bwrap",
  cpu: 1,
  memory: "256Mi",
  pids: 32,
  disk: "256Mi", // runaway intermediates become DiskQuotaExceeded, not a full volume
  ttl: "5m",
  network: "none", // shared data is not for redistributing: no way out
});

try {
  await sandbox.writeFile("/workspace/analysis.py", script);
  await sandbox.writeFile("/workspace/data.csv", data);

  let result;
  try {
    result = await sandbox.exec({
      command: "python3",
      args: ["analysis.py", "data.csv"],
      cwd: "/workspace",
      timeoutSeconds: 60,
      stdin: data,
    });
  } catch (error) {
    // Killed run (timeout, OOM, disk abuse): report it as an outcome, with
    // whatever partial output the analysis managed to emit.
    const partial = error.body ?? {};
    console.error(`--- run killed ---\n${error.message}`);
    process.stdout.write(partial.stdout ?? "");
    process.exit(1);
  }

  console.log("--- output ---");
  process.stdout.write(result.stdout);
  process.stderr.write(result.stderr);
  console.log(`--- run ---\nexit=${result.exitCode} reason=${result.reason ?? "EXITED"}`);

  // Receipts: what the analysis cost and what it did.
  const stats = await sandbox.stats();
  console.log(
    `cpu_millis=${stats.cpuMillis ?? 0} mem_peak=${stats.memoryPeakBytes ?? 0} ` +
      `disk_limited=true (profile)`,
  );
  const execs = await sandbox.execs();
  for (const record of execs) {
    console.log(
      `audit: ${record.command} exit=${record.exitCode} cpu=${record.cpuMillis ?? 0}ms`,
    );
  }
} finally {
  await sandbox.destroy();
}
