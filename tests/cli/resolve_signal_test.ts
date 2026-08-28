import { writeFileSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { waitForInstance } from "../helpers/client.ts";
import {
  externalDef,
  missingID,
  startedID,
  uid,
  waitForExternalToken,
} from "../helpers/genctl.ts";
import { parkedTask } from "../helpers/external.ts";

// The two ways to answer an external task from the CLI — `resolve` by token, `signal` by
// instance id + task id. The `external-tasks` listing they used to be discovered through is
// gone; a token is derived from the instance (helpers/external.ts), which is what the listing
// was doing under the covers.
//
// Every test resolves what it parked, so nothing lingers for the other CLI suites.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

/** Apply an external-parking process and start one instance of it. */
function park(prefix: string): { name: string; id: string } {
  const name = uid(prefix);
  runCli(bin, ["apply", "-f", writeDefs([externalDef(name)])]);
  return { name, id: startedID(runCli(bin, ["run", name]).stdout) };
}

test("resolve — delivers a result by token and un-parks the instance", async () => {
  const { id } = park("resolve");
  const token = await waitForExternalToken(id);

  const r = runCli(bin, ["resolve", token, "--set", "approved=true"]);
  expect(r.ok).toBe(true);
  expect(r.stdout).toContain("resolved:");
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);

test("resolve — the three result sources: --set, --result and -f", async () => {
  for (const source of [
    ["--set", "approved=true"],
    ["--result", "{approved: true}"],
    null, // -f, built below
  ]) {
    const { id } = park("resolve_src");
    const token = await waitForExternalToken(id);

    let args = source;
    if (!args) {
      const file = join(tmpdir(), `genroc_result_${Date.now()}.json`);
      writeFileSync(file, JSON.stringify({ approved: true }), "utf8");
      args = ["-f", file];
    }
    expect(runCli(bin, ["resolve", token, ...args]).ok).toBe(true);
    expect(await waitForInstance(id)).toBe("completed");
  }
}, 40_000);

test("resolve -q — succeeds silently", async () => {
  const { id } = park("resolve_q");
  const token = await waitForExternalToken(id);

  const r = runCli(bin, ["resolve", token, "--set", "approved=true", "-q"]);
  expect(r.ok).toBe(true);
  expect(r.stdout.trim()).toBe("");
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);

test("resolve — a result failing result_schema is rejected and the task stays parked", async () => {
  const { id } = park("resolve_bad");
  const token = await waitForExternalToken(id);

  const r = runCli(bin, ["resolve", token, "--set", "approved=notabool"]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("result is not valid for this task");
  // Rejected, not consumed: the instance is still parked on the same wait.
  expect(await parkedTask(id)).toBeDefined();

  runCli(bin, ["resolve", token, "--set", "approved=true"]);
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);

test("resolve — an unknown or missing token is an error", () => {
  const unknown = runCli(bin, ["resolve", `${missingID}.deadbeef`, "--set", "approved=true"]);
  expect(unknown.ok).toBe(false);
  expect(unknown.stderr).toContain("genctl:");

  const none = runCli(bin, ["resolve"]);
  expect(none.ok).toBe(false);
  expect(none.stderr).toContain("usage: genctl resolve");
});

// ── signal ──────────────────────────────────────────────────────────────────────

test("signal — delivers by instance id + --task and reports delivery", async () => {
  const { id } = park("signal");
  await waitForExternalToken(id);

  const r = runCli(bin, ["signal", id, "--task", "approval", "--set", "approved=true"]);
  expect(r.ok).toBe(true);
  expect(r.stdout).toContain("signaled:");
  // (delivered) distinguishes an armed task from one that buffered the result.
  expect(r.stdout).toContain("(delivered)");
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);

test("signal @last -f — addresses @last and reads the result from a file", async () => {
  const { id } = park("signal_last");
  await waitForExternalToken(id);

  const file = join(tmpdir(), `genroc_signal_${Date.now()}.json`);
  writeFileSync(file, JSON.stringify({ approved: true }), "utf8");

  expect(runCli(bin, ["signal", "@last", "--task", "approval", "-f", file]).ok).toBe(true);
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);

test("signal — needs --task, and rejects a result failing result_schema", async () => {
  const { id } = park("signal_bad");
  await waitForExternalToken(id);

  const noTask = runCli(bin, ["signal", id, "--set", "approved=true"]);
  expect(noTask.ok).toBe(false);
  expect(noTask.stderr).toContain("usage: genctl signal");

  const badResult = runCli(bin, ["signal", id, "--task", "approval", "--set", "approved=notabool"]);
  expect(badResult.ok).toBe(false);
  expect(badResult.stderr).toContain("result is not valid for task");

  runCli(bin, ["signal", id, "--task", "approval", "--set", "approved=true"]);
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);
