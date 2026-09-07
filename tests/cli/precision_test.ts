import { beforeAll, expect, test } from "vitest";
import { join } from "path";
import { tmpdir } from "os";
import { writeFileSync } from "fs";
import { buildGenctlBinary, runCli } from "../helpers/cli.ts";
import { API_BASE, waitForInstance } from "../helpers/client.ts";

// Numbers must survive the whole path, not just the engine: YAML/JSON parsed by
// the CLI, uploaded, stored, evaluated, returned, and rendered back by the CLI.
// genctl used to break both ends of that — gopkg.in/yaml.v3 decodes an integer
// too large for int64 into a float64, so a long id was destroyed on upload before
// the request left the machine, and responses were decoded with a plain
// json.Unmarshal, so `get` rendered through float64 even when the server held the
// value exactly.
//
// Every assertion here reads the CLI's raw stdout text. Parsing it as JSON would
// be self-defeating: JavaScript numbers are float64 too, so JSON.parse would
// corrupt the values under test before the assertion could run.

let bin: string;

beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

function uid(prefix: string) {
  return `${prefix}_${crypto.randomUUID().replace(/-/g, "").slice(0, 8)}`;
}

// 54 digits — past int64, so yaml.v3 tags it !!float and collapses it.
const BIG_INT = "123748297583958759399485776859493938587768583992939858";
// The float64 form the old path produced, asserted absent so a regression that
// merely *looks* plausible still fails.
const BIG_INT_AS_FLOAT64 = "1.2374829758395876e+53";
// 2^53+1: the smallest integer float64 cannot represent, and its neighbour.
const BEYOND_FLOAT64 = "9007199254740993";
const FLOAT64_NEIGHBOUR = "9007199254740992";
const PRECISE_FRACTION = "123456789.123456789";

/**
 * Write raw YAML text. The shared writeDefs helper builds YAML from JS objects,
 * which cannot carry these values — a JS number is float64, so the fixture itself
 * would round them before genctl ever saw the file.
 */
function writeRawYaml(text: string): string {
  const path = join(
    tmpdir(),
    `genroc_prec_${Date.now()}_${Math.random().toString(36).slice(2)}.yaml`,
  );
  writeFileSync(path, text, "utf8");
  return path;
}

/** A definition whose schema default carries `literal`, mapped through to the output. */
function defaultCarryingDef(name: string, literal: string): string {
  return `name: ${name}
input_schema:
  type: object
  properties:
    data:
      type: array
      items:
        type: integer
      default: [${literal}]
tasks:
  - id: shape
    output: "$: map(input.data, num => { num: num })"
    switch: end
output: "$: outputs.shape"
`;
}

async function applyRunGet(def: string, name: string, extraRunArgs: string[] = []) {
  const applied = runCli(bin, ["apply", "-f", writeRawYaml(def)]);
  expect(applied.stderr, `apply failed: ${applied.stderr}`).toBe("");
  expect(applied.ok).toBe(true);

  // `run` with no --input sends null, which fails input validation; supply an
  // empty object unless the caller is providing the input itself.
  const suppliesInput = extraRunArgs.some((a) => a === "--input" || a === "--set");
  const runArgs = suppliesInput ? extraRunArgs : ["--input", "{}"];
  const started = runCli(bin, ["run", name, "-q", ...runArgs]);
  expect(started.ok, `run failed: ${started.stderr}`).toBe(true);
  const id = started.stdout.trim();

  expect(await waitForInstance(id, 10_000)).toBe("completed");
  const got = runCli(bin, ["get", id]);
  expect(got.ok).toBe(true);
  return got.stdout;
}

// The reported case, end to end: a long integer written as a schema default,
// carried through a map expression and rendered back by the CLI.
test("genctl — a large integer in a schema default survives apply → run → get", async () => {
  const name = uid("precdefault");
  const out = await applyRunGet(defaultCarryingDef(name, BIG_INT), name);

  expect(out).toContain(BIG_INT);
  expect(out).not.toContain(BIG_INT_AS_FLOAT64);
  // It must survive on both sides of the map, not just where it entered.
  expect(out.match(new RegExp(BIG_INT, "g"))?.length ?? 0).toBeGreaterThanOrEqual(2);
});

// 2^53+1 is the tightest case: the corrupted form differs by one digit, so an
// assertion that only checked "looks like a big number" would pass regardless.
test("genctl — an integer just past float64 range is not rounded to its neighbour", async () => {
  const name = uid("precneighbour");
  const out = await applyRunGet(defaultCarryingDef(name, BEYOND_FLOAT64), name);

  expect(out).toContain(BEYOND_FLOAT64);
  expect(out).not.toContain(FLOAT64_NEIGHBOUR);
});

test("genctl — a high-precision fraction in a schema default is not rounded", async () => {
  const name = uid("precfraction");
  const def = `name: ${name}
input_schema:
  type: object
  properties:
    amount:
      type: number
      default: ${PRECISE_FRACTION}
tasks:
  - id: pass
    output: { amount: "$: input.amount" }
    switch: end
output: "$: outputs.pass"
`;
  const out = await applyRunGet(def, name);
  expect(out).toContain(PRECISE_FRACTION);
});

// --input goes through the CLI's relaxed YAML parser, the same lossy path as the
// definition file did.
test("genctl — a large integer passed via --input survives", async () => {
  const name = uid("precinput");
  const def = `name: ${name}
input_schema:
  type: object
  properties:
    id: { type: integer }
  required: [id]
tasks:
  - id: pass
    output: { id: "$: input.id" }
    switch: end
output: "$: outputs.pass"
`;
  const out = await applyRunGet(def, name, ["--input", `{"id":${BIG_INT}}`]);

  expect(out).toContain(BIG_INT);
  expect(out).not.toContain(BIG_INT_AS_FLOAT64);
});

// --set parses each value through the same relaxed parser.
test("genctl — a large integer passed via --set survives", async () => {
  const name = uid("precset");
  const def = `name: ${name}
input_schema:
  type: object
  properties:
    id: { type: integer }
  required: [id]
tasks:
  - id: pass
    output: { id: "$: input.id" }
    switch: end
output: "$: outputs.pass"
`;
  const out = await applyRunGet(def, name, ["--set", `id=${BIG_INT}`]);

  expect(out).toContain(BIG_INT);
  expect(out).not.toContain(BIG_INT_AS_FLOAT64);
});

// Arithmetic on a value that arrived through the CLI stays exact, and the decimal
// artefacts of a float64 pipeline (0.30000000000000004) never appear.
test("genctl — arithmetic on CLI-supplied numbers is exact", async () => {
  const name = uid("precmath");
  const def = `name: ${name}
input_schema:
  type: object
  properties:
    a: { type: number }
    b: { type: number }
    big: { type: integer }
  required: [a, b, big]
tasks:
  - id: calc
    output:
      sum: "$: input.a + input.b"
      exact: "$: input.a + input.b == 0.3"
      bigPlusOne: "$: input.big + 1"
    switch: end
output: "$: outputs.calc"
`;
  const out = await applyRunGet(def, name, [
    "--input",
    `{"a":0.1,"b":0.2,"big":${BEYOND_FLOAT64}}`,
  ]);

  expect(out).toContain(`"sum": 0.3`);
  expect(out).not.toContain("0.30000000000000004");
  expect(out).toContain(`"exact": true`);
  expect(out).toContain(`"bigPlusOne": 9007199254740994`);
});

// The --json rendering is a separate code path from the human-readable one.
test("genctl — get --json preserves the exact literal", async () => {
  const name = uid("precjson");
  const def = defaultCarryingDef(name, BIG_INT);
  runCli(bin, ["apply", "-f", writeRawYaml(def)]);
  const id = runCli(bin, ["run", name, "-q", "--input", "{}"]).stdout.trim();
  expect(await waitForInstance(id, 10_000)).toBe("completed");

  const got = runCli(bin, ["get", id, "--json"]);
  expect(got.ok).toBe(true);
  expect(got.stdout).toContain(BIG_INT);
  expect(got.stdout).not.toContain(BIG_INT_AS_FLOAT64);
});

// The audit trail is a third rendering path, and its payload column is read back and
// re-marshalled by the server before genctl ever sees it.
test("genctl — a literal in a log payload survives the trail", async () => {
  const name = uid("preclog");
  const def = defaultCarryingDef(name, BIG_INT);
  runCli(bin, ["apply", "-f", writeRawYaml(def)]);
  const id = runCli(bin, ["run", name, "-q", "--input", "{}"]).stdout.trim();
  expect(await waitForInstance(id, 10_000)).toBe("completed");

  // A width the payload fits in: `logs` cuts a row to the terminal, which would truncate
  // the literal under test rather than round it.
  const text = runCli(bin, ["logs", id], { COLUMNS: "5000" });
  expect(text.ok, text.stderr).toBe(true);
  expect(text.stdout).toContain(BIG_INT);
  expect(text.stdout).not.toContain(BIG_INT_AS_FLOAT64);

  const json = runCli(bin, ["logs", id, "--mode", "json"]);
  expect(json.stdout).toContain(BIG_INT);
  expect(json.stdout).not.toContain(BIG_INT_AS_FLOAT64);
});

// Past 1 MiB per object (api maxInlineResolveBytes) the server cannot carry the value in the
// response, so genctl fetches and splices it: the one display path where a literal is decoded
// from a second response. Built and posted as raw text -- JSON.stringify of 25k JS numbers
// would round every one of them before the server saw it.
test("genctl — a literal inside an object too large for the server to splice survives --resolve", async () => {
  const name = uid("precbig");
  const def = `name: ${name}
input_schema:
  type: object
  properties:
    rows: { type: array }
tasks:
  - id: pass
    switch: end
`;
  runCli(bin, ["apply", "-f", writeRawYaml(def)]);
  const started = await fetch(`${API_BASE}/instances`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: `{"process":"${name}","input":{"rows":[${Array(25_000).fill(BIG_INT).join(",")}]}}`,
  });
  expect(started.status).toBe(200);
  const id = ((await started.json()) as { id: string }).id;
  expect(await waitForInstance(id, 10_000)).toBe("completed");

  const resolved = runCli(bin, ["get", id, "--resolve"]);
  expect(resolved.ok, resolved.stderr).toBe(true);
  expect(resolved.stdout).toContain(BIG_INT);
  expect(resolved.stdout).not.toContain(BIG_INT_AS_FLOAT64);
}, 20_000);

// A value the server CAN carry is spliced before it leaves, so this is the same literal
// through the other half of --resolve.
test("genctl — a literal inside an externalized value survives --resolve", async () => {
  const name = uid("precobj");
  // No single leaf is large, so the cut has to take the array whole and the literals ride
  // out to the object store inside it.
  const rows = Array(100).fill(BIG_INT).join(",");
  const def = `name: ${name}
input_schema:
  type: object
  properties:
    rows: { type: array }
tasks:
  - id: pass
    switch: end
`;
  runCli(bin, ["apply", "-f", writeRawYaml(def)]);
  const id = runCli(bin, ["run", name, "-q", "--input", `{"rows":[${rows}]}`]).stdout.trim();
  expect(await waitForInstance(id, 10_000)).toBe("completed");

  // The unresolved view names the object rather than carrying it.
  const marked = runCli(bin, ["get", id]);
  expect(marked.stdout).toMatch(/"rows": \{\s*"ref": "[0-9a-f]{32}"/);

  const resolved = runCli(bin, ["get", id, "--resolve"]);
  expect(resolved.ok, resolved.stderr).toBe(true);
  expect(resolved.stdout).toContain(BIG_INT);
  expect(resolved.stdout).not.toContain(BIG_INT_AS_FLOAT64);
}, 15_000);

// A YAML spelling JSON cannot express must still be accepted rather than becoming
// an unmarshalable json.Number — the fallback in yamlToAny.
test("genctl — hex and leading-zero literals in a definition still apply", async () => {
  const name = uid("prechex");
  const def = `name: ${name}
input_schema:
  type: object
  properties:
    n: { type: integer, default: 0x1F }
tasks:
  - id: pass
    output: { n: "$: input.n" }
    switch: end
output: "$: outputs.pass"
`;
  const out = await applyRunGet(def, name);
  // 0x1F is 31; the point is that it applies and runs at all.
  expect(out).toContain("31");
});
