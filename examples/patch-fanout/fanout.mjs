// patch-fanout: one prepared repo state -> snapshot -> N forks -> evaluate one
// candidate patch per fork, concurrently. The snapshot/fork fan-out shape
// (ADR 023): forks are independent sandboxes that start from the same state.
import { Vora } from "@vorash/sdk";

const vora = new Vora();

// Agent-proposed candidates for a failing test. Untrusted, like all
// agent output — each one runs in its own sandbox.
const CANDIDATES = [
  { name: "candidate-A (strip first)", code: "def slug(s):\n    return s.strip().lower().replace(' ', '-')\n" },
  { name: "candidate-B (regex)", code: "import re\ndef slug(s):\n    return re.sub(r'[^a-z0-9]+', '-', s.lower()).strip('-')\n" },
  { name: "candidate-C (broken)", code: "def slug(s):\n    return s\n" },
  { name: "candidate-D (hangs)", code: "def slug(s):\n    while True: pass\n" },
];

const repoState = {
  "solution.py": "def slug(s):\n    return s.lower().replace(' ', '-')  # bug: no strip\n",
  "test_solution.py":
    "from solution import slug\n" +
    "assert slug('  Hello World  ') == 'hello-world', slug('  Hello World  ')\n" +
    "assert slug('a b') == 'a-b'\n" +
    "print('all tests pass')\n",
};

const spec = {
  image: "vora/python",
  backend: process.env.VORA_BACKEND ?? "bwrap",
  cpu: 0.5,
  memory: "128Mi",
  pids: 32,
  ttl: "10m",
  network: "none", // candidate code is untrusted: no egress at all
};

async function evaluate(sandbox, candidate) {
  try {
    await sandbox.writeFile("/workspace/solution.py", candidate.code);
    let result;
    try {
      result = await sandbox.exec({
        command: "python3",
        args: ["test_solution.py"],
        cwd: "/workspace",
        timeoutSeconds: 15,
      });
    } catch (error) {
      // Killed run: the stream ended with no exit record (hang starved at the
      // boundary). That is a verdict in itself — never a silent pass.
      return { candidate: candidate.name, verdict: "killed", reason: String(error.message), cpuMillis: 0, memoryPeakBytes: 0 };
    }
    const stats = await sandbox.stats();
    return {
      candidate: candidate.name,
      verdict: result.exitCode === 0 ? "FIXED" : "fails",
      reason: result.reason ?? "EXITED",
      cpuMillis: stats.cpuMillis ?? 0,
      memoryPeakBytes: stats.memoryPeakBytes ?? 0,
    };
  } catch (error) {
    return { candidate: candidate.name, verdict: "error", reason: String(error) };
  }
}

const source = await vora.sandboxes.create(spec);
let snapshot;
const forks = [];
try {
  for (const [path, content] of Object.entries(repoState)) {
    await source.writeFile(`/workspace/${path}`, content);
  }

  // The fan-out point: one live, crash-consistent capture of the state.
  snapshot = await vora.snapshots.create(source.id);
  console.log(`snapshot ${snapshot.id} (${snapshot.info.bytes} bytes) from ${source.id}`);

  // The snapshot outlives its source — destroy it before forking, like an
  // overnight fan-out would.
  await source.destroy();

  // One independent sandbox per candidate, all starting from the same state.
  for (const candidate of CANDIDATES) {
    forks.push([candidate, await snapshot.fork()]);
  }

  const results = await Promise.all(forks.map(([candidate, fork]) => evaluate(fork, candidate)));

  console.table(
    results.map((r) => ({
      candidate: r.candidate,
      verdict: r.verdict,
      reason: r.reason,
      cpu_ms: r.cpuMillis,
      mem_peak: r.memoryPeakBytes,
    })),
  );
} finally {
  // Fan-out is ephemeral: copies and snapshot go when the verdict is in.
  await Promise.all(forks.map(([, fork]) => fork.destroy().catch(() => {})));
  await snapshot?.remove().catch(() => {});
  await source.destroy().catch(() => {});
}
