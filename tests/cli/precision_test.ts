import { beforeAll, expect, test } from "vitest";
import { join } from "path";
import { tmpdir } from "os";
import { writeFileSync } from "fs";
import { buildGenctlBinary, runCli } from "../helpers/cli.ts";
import { API_BASE, waitForInstance } from "../helpers/client.ts";
import { uid } from "../helpers/genctl.ts";

// Assertions read raw stdout: JSON.parse would round the values under test to float64 first.

let bin: string;

beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

// 54 digits — past int64, so yaml.v3 tags it !!float and collapses it.
const BIG_INT = "123748297583958759399485776859493938587768583992939858";
// The float64 form, asserted absent so a plausible-looking regression still fails.
const BIG_INT_AS_FLOAT64 = "1.2374829758395876e+53";
// 2^53+1: the smallest integer float64 cannot represent, and its neighbour.
const BEYOND_FLOAT64 = "9007199254740993";
const FLOAT64_NEIGHBOUR = "9007199254740992";
const PRECISE_FRACTION = "123456789.123456789";

/** Not writeDefs: a JS-object fixture would round these values before genctl saw them. */
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

  // `run` with no --input sends null, which fails input validation.
  const suppliesInput = extraRunArgs.some((a) => a === "--input" || a === "--set");
  const runArgs = suppliesInput ? extraRunArgs : ["--input", "{}"];
  const started = runCli(bin, ["run", name, "-q", ...runArgs]);
  expect(started.ok, `run failed: ${started.stderr}`).toBe(true);
  const id = started.stdout.trim();

  expect(await waitForInstance(id, 10_000)).toBe("completed");
  const got = runCli(bin, ["detail", id]);
  expect(got.ok).toBe(true);
  return got.stdout;
}

test("genctl — a large integer in a schema default survives apply → run → detail", async () => {
  const name = uid("precdefault");
  const out = await applyRunGet(defaultCarryingDef(name, BIG_INT), name);

  expect(out).toContain(BIG_INT);
  expect(out).not.toContain(BIG_INT_AS_FLOAT64);
  // It must survive on both sides of the map, not just where it entered.
  expect(out.match(new RegExp(BIG_INT, "g"))?.length ?? 0).toBeGreaterThanOrEqual(2);
});

test("genctl — an integer just past float64 range is not rounded to its neighbour", async () => {
  const name = uid("precneighbour");
  const out = await applyRunGet(defaultCarryingDef(name, BEYOND_FLOAT64), name);

  expect(out).toContain(BEYOND_FLOAT64);
  expect(out).not.toContain(FLOAT64_NEIGHBOUR);
});

test("genctl — an integer past float64 written as a shape literal is not rounded", async () => {
  const name = uid("precshape");
  const def = `name: ${name}
tasks:
  - id: pass
    output: { id: ${BEYOND_FLOAT64}, ratio: 1.10 }
    switch: end
output: "$: outputs.pass"
`;
  const out = await applyRunGet(def, name);
  expect(out, "a literal in a shape must reach the stored definition and the output exactly").toContain(
    BEYOND_FLOAT64,
  );
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

// --input goes through the CLI's relaxed YAML parser.
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

// --set parses each value through the same parser.
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

  // Quoted would make it a string: the other way the YAML rendering loses the value.
  expect(out).toContain("sum: 0.3");
  expect(out).not.toContain("0.30000000000000004");
  expect(out).not.toContain(`sum: "0.3"`);
  expect(out).toContain("exact: true");
  expect(out).toContain("bigPlusOne: 9007199254740994");
});

// `schema` answers offline, so a literal printed back wrong was destroyed by the renderer alone.
test("genctl schema — a large default survives the round trip in both renderings", () => {
  const name = uid("precschema");
  const file = writeRawYaml(`name: ${name}
input_schema:
  type: object
  properties:
    n:
      type: number
      default: ${BEYOND_FLOAT64}
    ratio:
      type: number
      default: 1.10
tasks:
  - id: pass
    output:
      v: "$: input.n"
    switch:
      - goto: end
output:
  v: "$: outputs.pass.v"
`);

  for (const args of [[], ["--json"]]) {
    const label = args.length ? "--json" : "yaml";
    const got = runCli(bin, ["schema", "type", name, "input", "-f", file, ...args]);
    expect(got.ok, got.stderr).toBe(true);
    expect(got.stdout, `${label}: the default came back as a different number`).toContain(
      BEYOND_FLOAT64,
    );
    expect(got.stdout, `${label}: rounded to the float64 neighbour`).not.toContain(
      FLOAT64_NEIGHBOUR,
    );
    expect(got.stdout, `${label}: a trailing zero is part of what was written`).toContain("1.10");
  }

  // YAML has a second way to lose it: a number that comes back quoted is a string.
  const yaml = runCli(bin, ["schema", "type", name, "input", "-f", file]).stdout;
  expect(yaml).not.toContain(`"${BEYOND_FLOAT64}"`);
  expect(yaml, "the schema view is indented for pasting into a definition").toContain(
    "  input:\n",
  );
});

test("genctl — get --json preserves the exact literal", async () => {
  const name = uid("precjson");
  const def = defaultCarryingDef(name, BIG_INT);
  runCli(bin, ["apply", "-f", writeRawYaml(def)]);
  const id = runCli(bin, ["run", name, "-q", "--input", "{}"]).stdout.trim();
  expect(await waitForInstance(id, 10_000)).toBe("completed");

  const got = runCli(bin, ["detail", id, "--json"]);
  expect(got.ok).toBe(true);
  expect(got.stdout).toContain(BIG_INT);
  expect(got.stdout).not.toContain(BIG_INT_AS_FLOAT64);
});

// The server re-marshals the log payload column before genctl sees it.
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

  const json = runCli(bin, ["logs", id, "--json"]);
  expect(json.stdout).toContain(BIG_INT);
  expect(json.stdout).not.toContain(BIG_INT_AS_FLOAT64);
});

// Past 1 MiB (api maxInlineResolveBytes) genctl fetches and splices the object itself.
// Posted as raw text: JSON.stringify would round every number before the server saw it.
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

  const resolved = runCli(bin, ["detail", id, "--resolve"]);
  expect(resolved.ok, resolved.stderr).toBe(true);
  expect(resolved.stdout).toContain(BIG_INT);
  expect(resolved.stdout).not.toContain(BIG_INT_AS_FLOAT64);
}, 20_000);

// Under the cap the server splices it: the other half of --resolve.
test("genctl — a literal inside an externalized value survives --resolve", async () => {
  const name = uid("precobj");
  // No single leaf is large, so the cut externalizes the array whole.
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
  const marked = runCli(bin, ["detail", id]);
  expect(marked.stdout).toMatch(/rows:\s+ref: [0-9a-f]{32}/);

  const resolved = runCli(bin, ["detail", id, "--resolve"]);
  expect(resolved.ok, resolved.stderr).toBe(true);
  expect(resolved.stdout).toContain(BIG_INT);
  expect(resolved.stdout).not.toContain(BIG_INT_AS_FLOAT64);
}, 15_000);

// defdoc's `scalar` fallback: a spelling JSON cannot express must not become a json.Number.
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
