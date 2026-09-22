#!/usr/bin/env node
import { readFileSync } from "node:fs";

import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { Vora } from "@vorash/sdk";

import { createServer } from "./server.js";

// The version is read from the packaged manifest rather than duplicated here.
const manifest = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8")) as { version: string };

const server = createServer(new Vora(), { version: manifest.version });
await server.connect(new StdioServerTransport());

// stdout carries the JSON-RPC stream, so diagnostics go to stderr.
if (process.env.VORA_API_KEY === undefined || process.env.VORA_API_KEY === "") {
  console.error("vora-mcp: VORA_API_KEY is unset; the API will reject authenticated calls");
}