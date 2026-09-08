import { beforeAll, expect, test } from "vitest";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { buildGenctlBinary, runCli } from "../helpers/cli.ts";
import { uid } from "../helpers/genctl.ts";

// A rejected definition used to print prose with the location inside it, so nothing could
// jump to the line. The slot address now travels with the diagnostic and genctl turns it back
// into a position using the index it parsed the file with. specs/language-server.md §2, §3.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

function writeSource(body: string): string {
  const path = join(tmpdir(), `genroc_located_${Date.now()}_${Math.random().toString(36).slice(2)}.genroc.yaml`);
  writeFileSync(path, body, "utf8");
  return path;
}

test("apply — every broken slot is reported as file:line:col", () => {
  const name = uid("located");
  //            1        2       3            4           5              6         7
  const path = writeSource(
    `name: ${name}\n` +      // 1
      `tasks:\n` +           // 2
      `  - id: a\n` +        // 3
      `    action:\n` +      // 4
      `      type: fetch\n` + // 5
      `      url: "$: nope.x"\n` + // 6
      `    switch: next\n` + // 7
      `  - id: b\n` +        // 8
      `    action:\n` +      // 9
      `      type: fetch\n` + // 10
      `      url: "$: alsonope.y"\n` + // 11
      `    switch: end\n`,   // 12
  );

  const r = runCli(bin, ["apply", "--check-only", "-f", path]);
  expect(r.exitCode).toBe(1);

  const lines = r.stderr.trim().split("\n");
  // Both, not just the first: inference used to stop at the failure it found.
  expect(lines).toHaveLength(2);
  for (const line of lines) expect(line.startsWith("genctl: ")).toBe(true);

  // The action slot of each task, which yaml reports at its first key.
  expect(lines[0]).toContain(`${path}:5:7:`);
  expect(lines[0]).toContain(`field "nope" not found`);
  expect(lines[1]).toContain(`${path}:10:7:`);
  expect(lines[1]).toContain(`field "alsonope" not found`);
});

test("apply — a missing required field points at the node that lacks it", () => {
  // `tasks[0].id` has no node of its own, so the location is the task it is missing from.
  const name = uid("nofield");
  const path = writeSource(`name: ${name}\ntasks:\n  - action:\n      type: fetch\n      url: u\n    switch: end\n`);
  const r = runCli(bin, ["apply", "--check-only", "-f", path]);
  expect(r.exitCode).toBe(1);
  expect(r.stderr).toContain(`${path}:3:5:`);
  expect(r.stderr).toContain("id is required");
});
