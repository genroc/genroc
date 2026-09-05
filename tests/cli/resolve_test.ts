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
import { waitForParked } from "../helpers/external.ts";

// `resolve` answers an external task, addressed either way it can be: by the queue token a
// worker claimed it with, or by instance id + --task. The `external-tasks` listing tasks used
// to be discovered through is gone; a token is derived from the instance
// (helpers/external.ts), which is what the listing was doing under the covers.
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
  // Rejected, not consumed: the instance is still parked on the same wait. Polled rather than
  // read once — the shared server advances on its own clock, and a consumed task never comes
  // back parked, so this still fails loudly if the rejection did resolve it.
  await waitForParked(id, undefined, 2_000);

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

// ── resolve, addressed by instance id ───────────────────────────────────────────

test("resolve <id> --task — delivers by instance id and reports delivery", async () => {
  const { id } = park("signal");
  await waitForExternalToken(id);

  const r = runCli(bin, ["resolve", id, "--task", "approval", "--set", "approved=true"]);
  expect(r.ok).toBe(true);
  expect(r.stdout).toContain("resolved:");
  // (delivered) distinguishes an armed task from one that buffered the result.
  expect(r.stdout).toContain("(delivered)");
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);

test("resolve @last -f — addresses @last and reads the result from a file", async () => {
  const { id } = park("signal_last");
  await waitForExternalToken(id);

  const file = join(tmpdir(), `genroc_signal_${Date.now()}.json`);
  writeFileSync(file, JSON.stringify({ approved: true }), "utf8");

  expect(runCli(bin, ["resolve", "@last", "--task", "approval", "-f", file]).ok).toBe(true);
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);

// The argument decides the endpoint, so a half-named address is refused rather than sent:
// neither call can do anything with an instance id carrying no task, or a token carrying one.
test("resolve — an address that names half a task is refused, both ways round", async () => {
  const { id } = park("resolve_half");
  const token = await waitForExternalToken(id);

  const noTask = runCli(bin, ["resolve", id, "--set", "approved=true"]);
  expect(noTask.ok).toBe(false);
  expect(noTask.stderr).toContain("needs --task");

  const bothWays = runCli(bin, ["resolve", token, "--task", "approval", "--set", "approved=true"]);
  expect(bothWays.ok).toBe(false);
  expect(bothWays.stderr).toContain("already names one task");

  // Neither reached the server: the task is still parked and still resolvable.
  expect(runCli(bin, ["resolve", token, "--set", "approved=true"]).ok).toBe(true);
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);

test("resolve <id> — needs --task, and rejects a result failing result_schema", async () => {
  const { id } = park("signal_bad");
  await waitForExternalToken(id);

  // An id addresses an instance, not a task: without --task the command names no slot to
  // deliver to, and a queue token would have named one by itself.
  const noTask = runCli(bin, ["resolve", id, "--set", "approved=true"]);
  expect(noTask.ok).toBe(false);
  expect(noTask.stderr).toContain("needs --task");

  const badResult = runCli(bin, ["resolve", id, "--task", "approval", "--set", "approved=notabool"]);
  expect(badResult.ok).toBe(false);
  expect(badResult.stderr).toContain("result is not valid");

  runCli(bin, ["resolve", id, "--task", "approval", "--set", "approved=true"]);
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);
