// Registers weather-logger and tails one instance of it.
//
// The process is INFINITE by design (c1d53bd): it reads the weather every ten seconds and
// never finishes, so this prints each reading as it lands rather than waiting for an output.
// Ctrl-C pauses the instance, so the playground leaves nothing advancing behind it.
//
// Two terminals besides this one:
//   go run ./cmd/genroc -db tests/playground/genroc.db --http :8888   # the engine
//   npm run playground:scripts                                       # the evaluator worker,
//       # which CLAIMS script tasks off :8888 — it listens on nothing
//
// The reading task's code lives in reading.ts, pulled in by `$import` — .genroc registers the
// resolver, and the apply below typechecks it against the generated Input/Output before the
// string exists. `genctl types -f script-node.yaml -f process.yaml` regenerates the
// declarations on their own, which is what an editor wants between applies.
//
// Usage: npm run playground:run -- [place]      (the `--` is npm's; without it the arg is eaten)

import { join } from "node:path";
import { spawnSync } from "node:child_process";
import { createClientTyped } from "../helpers/client.ts";
import { buildGenctlBinary } from "../helpers/cli.ts";
import type { ReadingOutput } from "./generated/types.ts";

const PROCESS_NAME = "weather-logger";
const SERVER = "http://localhost:8888";
const POLL_MS = 2000;

const place = process.argv[2] ?? "Praha";

const repoRoot = join(import.meta.dirname, "../..");
// Both, and the child first: the parent names `script-node`, so it must already exist.
const defs = ["script-node.yaml", "process.yaml"].map((f) =>
  join(import.meta.dirname, f),
);
const client = createClientTyped({ baseUrl: `${SERVER}/api` });

console.log(`\nRegistering "${PROCESS_NAME}"…`);
const reg = spawnSync(
  buildGenctlBinary(),
  ["apply", "--server", SERVER, ...defs.flatMap((f) => ["-f", f])],
  { cwd: repoRoot, encoding: "utf8", stdio: "inherit" },
);
if (reg.status !== 0) throw new Error("genctl apply failed");

console.log(`Starting one instance, reading ${place} every 10s. Ctrl-C to pause it.\n`);
const { data: started, error: startErr } = await client.POST("/instances", {
  body: { process: PROCESS_NAME, input: { place } },
});
if (startErr) throw new Error(`start failed: ${JSON.stringify(startErr)}`);
const id = started!.id;

// Pause rather than leave it running: an infinite process outlives this terminal, and the
// next `playground:run` would then be competing with it for the evaluator.
let stopping = false;
process.on("SIGINT", async () => {
  if (stopping) process.exit(130);
  stopping = true;
  await client.POST("/instances/{id}/pause", { params: { path: { id } } });
  console.log(`\npaused ${id}`);
  console.log(`  genctl get ${id} --server ${SERVER}`);
  console.log(`  genctl resume ${id} --server ${SERVER}`);
  process.exit(0);
});

let seen = -1;
for (;;) {
  const { data } = await client.GET("/instances/{id}/detail", {
    params: { path: { id } },
  });
  // `outputs` is a bag on the wire — the generated types describe the SLOTS, not the map.
  const outputs = data?.state?.outputs as Record<string, unknown> | undefined;
  const reading = outputs?.reading as ReadingOutput | undefined;
  // `count` accumulates across loop iterations, so it is also the cursor: printing on a change
  // rather than on every poll keeps one line per reading however often this polls.
  if (reading && reading.count !== seen) {
    seen = reading.count;
    console.log(`${String(reading.count).padStart(3)}  ${reading.last.summary}`);
  }
  if (data?.status && !["running", "pausing"].includes(data.status)) {
    console.log(`\n${id} → ${data.status}`);
    console.log(`${data.error_code ?? ""} ${data.error_message ?? ""}`.trim());
    break;
  }
  await new Promise((r) => setTimeout(r, POLL_MS));
}
