import { beforeAll, describe, expect, test } from "vitest";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { uid } from "../helpers/genctl.ts";
import { client } from "../helpers/client.ts";
import {
  assertUniqueNames,
  loadGroup,
  writeExpected,
  type CompatCase,
} from "../helpers/compat-fixtures.ts";

// The compat report, asserted as the whole rendered output. What an operator reads IS the
// deliverable here — the verdict is blind to meaning, so a report they cannot act on is not
// a feature — and comparing the whole thing covers layout, wording, ordering and exit code
// at once.
//
// The one row no case here can produce is `unanalysable`: reaching it needs a stored version
// that fails its own inference, which nothing can apply. It lives in
// internal/validation/unanalysable_test.go, §5's rule that it cannot be excused included.
//
// One case per file in testdata/compat/<group>/, each with its expected block at the end.
// Adding one is a new file plus `UPDATE_COMPAT=1 vitest run cli/compat_test.ts`. Read the
// resulting block before committing it: a regenerated expectation records whatever the code
// does, including a bug.

const GROUPS = ["shapes", "children", "resolution", "submitted", "wire"];
const UPDATING = process.env.UPDATE_COMPAT === "1";

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

/**
 * Apply the case's definitions, run its compat command, and return what the operator would
 * see. stderr and the exit code are part of it — a refusal that stopped failing the build
 * would otherwise pass silently.
 */
function runCase(c: CompatCase): string {
  for (const step of c.apply) {
    const applied = runCli(bin, ["apply", "-f", writeDefs(step.definitions), "--channel", step.channel]);
    if (!applied.ok) {
      // A fixture that fails to apply would otherwise have its expected block recorded
      // from an equally broken run.
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

/**
 * The instance-id form. Both sides of the comparison are already on the row — its process, at
 * the version it is on — so the operator names only the target: `compat <id> --to N` is the
 * question `upgrade <id> --to N` answers by moving. Not a fixture case: these need a live
 * instance, which the golden harness has no way to create.
 */
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

  async function start(name: string): Promise<string> {
    const { data, error } = await client.POST("/instances", {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      body: { process: name, input: {} } as any,
    });
    expect(error).toBeUndefined();
    return data!.id;
  }

  test("names only the target, and the row's process scopes the report", async () => {
    const mine = uid("compatid");
    const other = uid("compatother");
    runCli(bin, ["apply", "-f", writeDefs([held(mine, false), held(other, false)])]);
    const id = await start(mine);
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

    // A channel carries every process on it. The row names one, so that is what is compared:
    // a report covering the whole channel would answer a question nobody asked.
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
    const id = await start(name);

    const r = runCli(bin, ["compat", id, "-f", writeDefs([held(name, true)])]);
    expect(r.ok, r.stderr).toBe(true);
    expect(r.stdout).toContain(`${name}  v1 → (new)`);
  });

  test("refuses a side the id already names", async () => {
    const name = uid("compatidargs");
    runCli(bin, ["apply", "-f", writeDefs([held(name, false)])]);
    const id = await start(name);

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
