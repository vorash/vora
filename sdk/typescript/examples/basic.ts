/**
 * End-to-end example against a running vora-api.
 *
 *   export VORA_API_URL=http://127.0.0.1:9300
 *   export VORA_API_KEY=vora_...
 *   npm run build && npm run example
 */
import { Vora } from "../dist/esm/index.js";

const vora = new Vora();

const sandbox = await vora.sandboxes.create({
  image: "vora/python",
  memory: "512Mi",
  cpu: 1,
  pids: 128,
  network: "none",
  ttl: "10m",
});

console.log(`created ${sandbox.id} (${sandbox.info.state ?? "unknown"})`);

try {
  await sandbox.writeFile("/workspace/main.py", 'import sys\nprint("hello from vora")\nprint(sys.stdin.read().strip())\n');

  const result = await sandbox.exec("python /workspace/main.py", { stdin: "piped in" });
  process.stdout.write(result.stdout);
  process.stderr.write(result.stderr);
  console.log(`exit=${result.exitCode}`);

  const stats = await sandbox.stats();
  console.log(`memory=${stats.memoryBytes ?? 0} bytes cpu=${stats.cpuMillis ?? 0} millis`);
} finally {
  await sandbox.destroy();
  console.log("destroyed");
}