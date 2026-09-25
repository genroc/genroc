import { writeFileSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { waitForInstance } from "../helpers/client.ts";
import { externalDef, missingID, startedID, uid, waitForExternalToken } from "../helpers/genctl.ts";

// Every test resolves what it parked, so nothing lingers for the other CLI suites.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

function start(def: { name: string }): string {
  runCli(bin, ["apply", "-f", writeDefs([def])]);
  return startedID(runCli(bin, ["run", def.name]).stdout);
}

test("signal <id> --task — delivers to an armed task and says so", async () => {
  const id = start(externalDef(uid("signal")));
  await waitForExternalToken(id);

  const r = runCli(bin, ["signal", id, "--task", "approval", "--set", "approved=true"]);
  expect(r.ok).toBe(true);
  expect(r.stdout, "an armed task takes the outcome at once").toContain("signaled:");
  expect(r.stdout).toContain("(delivered)");
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);

test("signal — before the task arms, the outcome is buffered and consumed when it does", async () => {
  const def = externalDef(uid("signal_early"));
  // A delay in front, so the signal lands while `approval` is not armed yet.
  def.tasks.unshift({ id: "wait", action: { type: "delay", for: 1500 }, switch: [{ goto: "next" }] } as any);
  const id = start(def);

  const r = runCli(bin, ["signal", id, "--task", "approval", "--set", "approved=true"]);
  expect(r.ok).toBe(true);
  expect(r.stdout, "an unarmed task must report the outcome as buffered, not delivered").toContain("(buffered)");
  expect(await waitForInstance(id, 20_000)).toBe("completed");
}, 30_000);

test("signal @last -f — addresses @last and reads the result from a file", async () => {
  const id = start(externalDef(uid("signal_last")));
  await waitForExternalToken(id);

  const file = join(tmpdir(), `genroc_signal_${Date.now()}.json`);
  writeFileSync(file, JSON.stringify({ approved: true }), "utf8");

  expect(runCli(bin, ["signal", "@last", "--task", "approval", "-f", file]).ok).toBe(true);
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);

test("signal --result -q — takes a literal and succeeds silently", async () => {
  const id = start(externalDef(uid("signal_q")));
  await waitForExternalToken(id);

  const r = runCli(bin, ["signal", id, "--task", "approval", "--result", "{approved: true}", "-q"]);
  expect(r.ok).toBe(true);
  expect(r.stdout.trim()).toBe("");
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);

test("signal — an unknown instance is an error", () => {
  const r = runCli(bin, ["signal", missingID, "--task", "approval", "--set", "approved=true"]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("genctl:");
});

test("signal — refuses a missing --task and a result failing result_schema", async () => {
  const id = start(externalDef(uid("signal_bad")));
  await waitForExternalToken(id);

  const noTask = runCli(bin, ["signal", id, "--set", "approved=true"]);
  expect(noTask.ok).toBe(false);
  expect(noTask.stderr).toContain("--task <task-id> is required");

  const badResult = runCli(bin, ["signal", id, "--task", "approval", "--set", "approved=notabool"]);
  expect(badResult.ok).toBe(false);
  expect(badResult.stderr).toContain("result is not valid");

  // Neither was consumed: the task still takes a good outcome.
  expect(runCli(bin, ["signal", id, "--task", "approval", "--set", "approved=true"]).ok).toBe(true);
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);
