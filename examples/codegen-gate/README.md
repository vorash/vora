# codegen-gate — accept AI-generated code only after it survives its tests

**The problem.** Your product generates code with an LLM, such as a data
transform, a regex, or a migration. The output is plausible and sometimes wrong,
sometimes loops endlessly, and is occasionally destructive. You want to *try* it before you
trust it, and "try" must not mean "run it on the application server".

**The move.** Use a small review service. Submit `{ code, tests }`, and the
service runs the code against the tests inside a sandbox and returns a verdict:
accepted, failed, or killed-by-budget. Only code that passed ever reaches your product.

## Run it

```bash
cd examples && npm install          # once
node codegen-gate/server.mjs        # http://127.0.0.1:9400
```

```bash
curl -s localhost:9400/review -d '{
  "code": "def slug(s):\n    return s.lower().replace(\" \", \"-\")\n",
  "tests": "from solution import slug\nassert slug(\"Hello World\") == \"hello-world\"\nassert slug(\"  a  \") == \"-a-\"\nprint(\"ok\")\n"
}'
# {"verdict":"accepted","exitCode":0,"reason":"Exited", ...}
```

Then try `"tests": "import time\nwhile True: time.sleep(1)\n"` and watch the
verdict come back `killed`. The runaway run is cut at the boundary, and the
SDK refuses to report a stream with no exit record as success. The gate fails
closed on misbehaving code instead of hanging your pipeline.

(`tests` is a plain assert script. The image ships no pytest and the sandbox
has no network to install one, which is the point.)

## What to notice

- Generated code is untrusted twice over: the model wrote it *and* the user
  provoked it. `network: "none"`, `pids: 32`, and a 30s exec timeout limit both
  the damage it can do and the worst-case latency of the gate.
- The verdict carries the semantic reason (`OOMKilled`, `TimeoutExceeded`, …),
  so the product can distinguish "wrong answer" from "hostile or broken" and
  re-prompt accordingly.
- `server.mjs` is dependency-free (`node:http`). The trust boundary does not
  depend on anything the code-generation stack ships.
