import { beforeAll, describe, expect, test } from "vitest";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
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
