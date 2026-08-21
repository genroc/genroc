// Registers weather-logger and runs one instance to completion.
//
// Two terminals besides this one:
//   go run ./cmd/genroc --http :8888   # the engine
//   bun run playground:scripts         # the script evaluator (bun-runtime/)
//
// The summarize task's code lives in summarize.ts, pulled in by `$import` — genroc.yaml
// registers the resolver, and the apply below typechecks it against generated Input/Output
// before the string exists. `genctl types -f script.yaml -f process.yaml` regenerates the
// declarations on their own, which is what an editor wants between applies.
//
// open-meteo is called by genroc itself, so there is no data-source process to start.
//
// Usage: bun run playground:run [ticks] [place]

import { join } from "node:path";
import { spawnSync } from "node:child_process";
import { createClientTyped, waitForInstance } from "../helpers/client.ts";
import { buildGenctlBinary } from "../helpers/cli.ts";
import type { ProcessOutput } from "./generated/types.ts";

const PROCESS_NAME = "weather-logger";
const SERVER = "http://localhost:8888";

const ticks = Number(process.argv[2] ?? 3);
const place = process.argv[3] ?? "Praha";

const repoRoot = join(import.meta.dirname, "../..");
// Both, and the child first: the parent names `script`, so it must already exist.
const defs = ["script.yaml", "process.yaml"].map((f) => join(import.meta.dirname, f));
const client = createClientTyped({ baseUrl: SERVER });

console.log(`\nRegistering "${PROCESS_NAME}"…`);
const reg = spawnSync(buildGenctlBinary(), ["apply", "--server", SERVER, ...defs.flatMap((f) => ["-f", f])], {
  cwd: repoRoot,
  encoding: "utf8",
  stdio: "inherit",
});
if (reg.status !== 0) throw new Error("genctl apply failed");

console.log(`Starting one instance: ${ticks} reading(s) of ${place}.`);
const { data: started, error: startErr } = await client.POST("/instances", {
  body: { process: PROCESS_NAME, input: { ticks, place } },
});
if (startErr) throw new Error(`start failed: ${JSON.stringify(startErr)}`);

// The process parks until the next whole 10 seconds before reading, so this takes a few
// seconds — no timeout here, since waiting on a wall clock is the design.
const status = await waitForInstance(started!.id, Infinity, client);
const { data } = await client.GET("/instances/{id}", { params: { path: { id: started!.id } } });

console.log(`\n${started!.id} → ${status}`);
if (status === "completed") {
  const out = data?.context?.output as ProcessOutput | undefined;
  console.log(`${out?.readings} reading(s)`);
  console.log(out?.summary);
} else {
  // The script's own error detail stays inside the child instance; what crosses the
  // boundary is a CODE, which is the whole point of the child's throws clause.
  console.log(`${data?.error_code}: ${data?.error}`);
}
console.log(JSON.stringify(data?.context, null, 2));
