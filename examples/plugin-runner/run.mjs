// plugin-runner: run an untrusted, user-uploaded data-transform plugin as part
// of a trusted pipeline. The plugin's only contract is stdin -> stdout; its
// world is one sandbox with a strict profile.
//
//   node plugin-runner/run.mjs <plugin-file> [input-file]
import { readFile } from "node:fs/promises";
import process from "node:process";

import { Vora } from "@vorash/sdk";

const PROFILES = {
  strict: { cpu: 0.5, memory: "64Mi", pids: 32, disk: "64Mi", network: "none" },
  standard: { cpu: 1, memory: "256Mi", pids: 64, disk: "256Mi", network: "none" },
  network: {
    cpu: 1,
    memory: "256Mi",
    pids: 64,
    disk: "256Mi",
    network: { mode: "restricted", allowedDomains: ["api.example.com"] },
  },
};

const [, , pluginPath, inputPath] = process.argv;
if (pluginPath === undefined) {
  console.error("usage: node run.mjs <plugin-file> [input-file]");
  process.exit(2);
}

const vora = new Vora();
const profile = PROFILES[process.env.PLUGIN_PROFILE ?? "strict"] ?? PROFILES.strict;

const plugin = await readFile(pluginPath, "utf8");
const input = inputPath === undefined ? "" : await readFile(inputPath, "utf8");

const sandbox = await vora.sandboxes.create({
  image: "vora/node",
  backend: process.env.VORA_BACKEND ?? "bwrap",
  ttl: "1m",
  ...profile,
});

try {
  await sandbox.writeFile("/workspace/plugin.mjs", plugin);
  // The plugin sees input on stdin and produces output on stdout — nothing
  // else is shared. The product's own code never loads the plugin.
  let result;
  try {
    result = await sandbox.exec({
      command: "node",
      args: ["plugin.mjs"],
      cwd: "/workspace",
      stdin: input,
      timeoutSeconds: 20,
    });
  } catch (error) {
    // Killed run (timeout, OOM, fork storm): surface it as a product error,
    // never as empty output that looks like a no-op transform.
    const partial = error.body ?? {};
    console.error(`[plugin run] killed: ${error.message}`);
    process.stdout.write(partial.stdout ?? "");
    process.exit(1);
  }

  const stats = await sandbox.stats();
  console.error(
    `[plugin run] exit=${result.exitCode} reason=${result.reason ?? "EXITED"} ` +
      `mem_peak=${stats.memoryPeakBytes ?? "?"} cpu_millis=${stats.cpuMillis ?? "?"}`,
  );
  process.stdout.write(result.stdout);
  if (result.exitCode !== 0) {
    process.stderr.write(result.stderr);
    process.exitCode = 1;
  }
} finally {
  await sandbox.destroy();
}
