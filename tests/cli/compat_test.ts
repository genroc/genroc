import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { uid } from "../helpers/genctl.ts";

// `compat` reports what changed between two versions and whether anything running can
// observe it. One endpoint shape, three ergonomics — the CLI hides the selector
// verbosity, not the API.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

/** Parks on an external task, so an instance sits at a known task for as long as needed. */
function parkingDef(name: string, shipURL: string, extra: Record<string, unknown> = {}) {
  return {
    name,
    ...extra,
    tasks: [
      {
        id: "approve",
        action: {
          type: "external",
          result_schema: {
            type: "object",
            properties: { ok: { type: "boolean" } },
            required: ["ok"],
          },
        },
        output: { ok: "$: self.result.ok" },
        switch: [{ goto: "next" }],
      },
      { id: "ship", action: { type: "fetch", url: shipURL }, switch: [{ goto: "end" }] },
    ],
  };
}

function applyDef(def: Record<string, unknown>): number {
  const out = runCli(bin, ["apply", "-f", writeDefs([def])]).stdout;
  const m = out.match(/@v(\d+)/);
  if (!m) throw new Error(`no version in: ${out}`);
  return Number(m[1]);
}

// ── compat ──────────────────────────────────────────────────────────────────────

test("compat — two stored versions of one process, as three positionals", () => {
  const name = uid("cli_compat");
  const v1 = applyDef(parkingDef(name, "http://localhost:9001/ship"));
  const v2 = applyDef(parkingDef(name, "http://localhost:9002/ship"));

  const { stdout } = runCli(bin, ["compat", name, String(v1), String(v2)]);
  expect(stdout).toContain("PROCESS");
  expect(stdout).toContain(`${name}`);
  expect(stdout).toMatch(/v1\s+v2\s+yes\s+yes/);
  // The changed slot is the deliverable, not the verdict.
  expect(stdout).toContain("action.url");
  // Every surface says what the check cannot see, in the user's words.
  expect(stdout).toContain("shape only");
});

test("compat — an incompatible pair carries a reason and exits non-zero", () => {
  const name = uid("cli_compat_bad");
  const v1 = applyDef(parkingDef(name, "http://localhost:9001/ship"));
  const v2 = applyDef(
    parkingDef(name, "http://localhost:9001/ship", {
      input_schema: {
        type: "object",
        properties: { currency: { type: "string" } },
        required: ["currency"],
      },
    }),
  );

  const { stdout, exitCode } = runCli(bin, ["compat", name, String(v1), String(v2)]);
  expect(stdout).toContain("NO");
  // A bare "incompatible" tells an operator nothing actionable.
  expect(stdout).toContain("newly required");
  expect(exitCode).toBe(1);
});

test("compat -f — submitted files against a channel, mirroring apply -f", () => {
  const name = uid("cli_compat_f");
  applyDef(parkingDef(name, "http://localhost:9001/ship"));
  const file = writeDefs([parkingDef(name, "http://localhost:9004/ship")]);

  const { stdout } = runCli(bin, ["compat", "-f", file, "--from", "latest", name]);
  // A submitted document has no version yet, so it reports as new rather than borrowing
  // the number an apply would assign.
  expect(stdout).toContain("(new)");
  expect(stdout).toContain("action.url");
});

test("compat — naming only one side is refused", () => {
  const { stderr, exitCode } = runCli(bin, ["compat", "--from", "latest"]);
  expect(stderr).toContain("usage");
  expect(exitCode).toBe(1);
});

test("compat --json — the raw report", () => {
  const name = uid("cli_compat_json");
  const v1 = applyDef(parkingDef(name, "http://localhost:9001/ship"));
  const v2 = applyDef(parkingDef(name, "http://localhost:9002/ship"));

  const parsed = JSON.parse(
    runCli(bin, ["compat", name, String(v1), String(v2), "--json"]).stdout,
  );
  expect(parsed.compatible).toBe(true);
  expect(parsed.processes[0].name).toBe(name);
});
