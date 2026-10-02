import { beforeAll, describe, expect, test } from "vitest";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { uid } from "../helpers/genctl.ts";
import { client, startInstance } from "../helpers/client.ts";
import {
  assertUniqueNames,
  loadGroup,
  writeExpected,
  type CompatCase,
} from "../helpers/compat-fixtures.ts";

// The compat report, asserted whole; `unanalysable` lives in internal/validation/unanalysable_test.go.
// New case: a file in testdata/compat/<group>/, then `UPDATE_COMPAT=1 vitest run cli/compat_test.ts`.
// Read the regenerated block before committing: it records whatever the code does, bugs included.

const GROUPS = ["shapes", "children", "resolution", "submitted", "wire"];
const UPDATING = process.env.UPDATE_COMPAT === "1";

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

/** stderr and the exit code are part of the report: a refusal that stopped failing would pass. */
function runCase(c: CompatCase): string {
  for (const step of c.apply) {
    const applied = runCli(bin, ["apply", "-f", writeDefs(step.definitions), "--channel", step.channel]);
    if (!applied.ok) {
      // Else UPDATE_COMPAT would record the expected block from a broken run.
      throw new Error(`apply failed for ${c.id}: ${applied.stderr || applied.stdout}`);
    }
  }
  const args = c.submit ? ["-f", writeDefs(c.submit), ...c.run] : c.run;
  const { stdout, stderr, exitCode } = runCli(bin, ["compat", ...args]);

  const parts: string[] = [];
  if (stdout.trim()) parts.push(stdout.trimEnd(), "");
  // A refusal prints nothing to stdout, so stderr is the whole report for those cases.
  if (stderr.trim()) parts.push("--- stderr ---", stderr.trimEnd(), "");
  parts.push(`exit ${exitCode}`);
  return parts.join("\n") + "\n";
}

assertUniqueNames(GROUPS);

for (const group of GROUPS) {
  describe(group, () => {
    for (const c of loadGroup(group)) {
      test(c.id.split("/")[1], () => {
        const got = runCase(c);
        if (UPDATING) {
          writeExpected(c, got);
          return;
        }
        expect(got).toBe(c.expect);
      });
    }
  });
}

/** `compat <id> --to N`, the row being the from side. Not fixtures: they need a live instance. */
describe("instance id", () => {
  const held = (name: string, tag: boolean) => ({
    name,
    input_schema: {
      type: "object",
      properties: {
        note: { type: ["string", "null"] },
        ...(tag ? { tag: { type: ["string", "null"] } } : {}),
      },
    },
    tasks: [{ id: "hold", action: { type: "external" }, switch: "end" }],
  });

  test("names only the target, and the row's process scopes the report", async () => {
    const mine = uid("compatid");
    const other = uid("compatother");
    runCli(bin, ["apply", "-f", writeDefs([held(mine, false), held(other, false)])]);
    const id = await startInstance(mine, {});
    runCli(bin, [
      "apply",
      "-f",
      writeDefs([held(mine, true), held(other, true)]),
      "--channel",
      "compatid_next",
    ]);

    const byVersion = runCli(bin, ["compat", id, "--to", "2"]);
    expect(byVersion.ok, byVersion.stderr).toBe(true);
    expect(byVersion.stdout).toContain(`${mine}  v1 → v2`);

    const byChannel = runCli(bin, ["compat", id, "--to", "compatid_next"]);
    expect(byChannel.ok, byChannel.stderr).toBe(true);
    expect(byChannel.stdout).toContain(`${mine}  v1 → v2`);
    expect(byChannel.stdout, "the report was not scoped to the process on the row").not.toContain(
      other,
    );
  });

  test("compares against a file that was never applied", async () => {
    const name = uid("compatidf");
    runCli(bin, ["apply", "-f", writeDefs([held(name, false)])]);
    const id = await startInstance(name, {});

    const r = runCli(bin, ["compat", id, "-f", writeDefs([held(name, true)])]);
    expect(r.ok, r.stderr).toBe(true);
    expect(r.stdout).toContain(`${name}  v1 → (new)`);
  });

  test("refuses a side the id already names", async () => {
    const name = uid("compatidargs");
    runCli(bin, ["apply", "-f", writeDefs([held(name, false)])]);
    const id = await startInstance(name, {});

    const withFrom = runCli(bin, ["compat", id, "--from", "compatid_next", "--to", "2"]);
    expect(withFrom.ok).toBe(false);
    expect(withFrom.stderr).toContain("drop --from");

    const two = runCli(bin, ["compat", id, id, "--to", "2"]);
    expect(two.ok).toBe(false);
    expect(two.stderr, "two rows are two comparisons, not one with two from sides").toContain(
      "one instance id",
    );

    const bothTargets = runCli(bin, [
      "compat",
      id,
      "-f",
      writeDefs([held(name, true)]),
      "--to",
      "2",
    ]);
    expect(bothTargets.ok).toBe(false);
    expect(bothTargets.stderr).toContain("drop --to");
  });
});
