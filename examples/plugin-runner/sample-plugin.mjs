// A benign data-transform plugin: normalize the "name" column of a CSV read
// from stdin, write the result to stdout. This is the entire plugin contract.
import { createInterface } from "node:readline";

const lines = createInterface({ input: process.stdin });
for await (const line of lines) {
  const [name, ...rest] = line.split(",");
  console.log([name.trim().toLowerCase().replace(/\s+/g, "-"), ...rest].join(","));
}
