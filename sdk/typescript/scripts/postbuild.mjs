// The root package.json sets "type": "module", so Node would read the tsc CJS
// output in dist/cjs as ESM. A marker file in that directory flips it back.
import { mkdirSync, writeFileSync } from "node:fs";

mkdirSync("dist/cjs", { recursive: true });
writeFileSync("dist/cjs/package.json", `${JSON.stringify({ type: "commonjs" }, null, 2)}\n`);