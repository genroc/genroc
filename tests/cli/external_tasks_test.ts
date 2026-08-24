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

// The external-task entity: the `external-tasks` queue that shows what is waiting, and
// the two ways to answer it — `resolve` by token, `signal` by instance id + task id.
//
// Every test resolves what it parked, so nothing lingers on the queue the other CLI
// suites read. That is also why the listCap notice is not re-tested here: parking 21 tasks
// to prove a cap would leave a mess for one more copy of behaviour that definitions_test
// and instances_test already pin, on top of fetchOrdered's own unit tests.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

type TaskRow = {
  token: string;
  process: string;
  version: number;
  task_id: string;
  input: unknown;
  result_schema: unknown;
  waiting_since: string;
};

function queue(extra: string[] = []): TaskRow[] {
  return JSON.parse(runCli(bin, ["external-tasks", "--json", ...extra]).stdout) as TaskRow[];
}

/** Apply an external-parking process and start one instance of it. */
function park(prefix: string): { name: string; id: string } {
  const name = uid(prefix);
  runCli(bin, ["apply", "-f", writeDefs([externalDef(name)])]);
  return { name, id: startedID(runCli(bin, ["run", name]).stdout) };
}

// ── the queue: displayed fields ─────────────────────────────────────────────────

test("external-tasks — the table commits to WAITING, PROCESS, TASK, CLAIMED BY and TOKEN", async () => {
  const { name, id } = park("etcols");
  const token = await waitForExternalToken(id);

  const lines = runCli(bin, ["external-tasks", "--process", name]).stdout.trim().split("\n");
  expect(lines[0].split(/\s\s+/)).toEqual(["WAITING", "PROCESS", "TASK", "CLAIMED BY", "TOKEN"]);

  const row = lines[1];
  expect(row).toContain("just now"); // WAITING is a relative age
  expect(row).toContain(`${name}@v1`);
  expect(row).toContain("approval");
  // A dash, not a blank: unclaimed work and a column that failed to render must not look the
  // same in a queue read to find work that is stuck.
  expect(row).toContain("-");
  // The token goes last because it is long, and it is what `resolve` takes.
  expect(row.trimEnd().endsWith(token)).toBe(true);

  runCli(bin, ["resolve", token, "--set", "approved=true"]);
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);

test("external-tasks --json — carries the input and result_schema the table omits", async () => {
  const { name, id } = park("etjson");
  await waitForExternalToken(id);

  const [task] = queue(["--process", name]);
  expect(task.task_id).toBe("approval");
  expect(task.input).toEqual({ msg: "approve me" });
  // The schema a resolver must satisfy is only reachable through --json.
  expect(task.result_schema).toMatchObject({ type: "object", required: ["approved"] });
  expect(task.waiting_since).toMatch(/^\d{4}-\d{2}-\d{2}T/);

  // The queue never exposes process context, only the task's own snapshot.
  const table = runCli(bin, ["external-tasks", "--process", name]).stdout;
  expect(table).not.toContain("approve me");

  runCli(bin, ["resolve", task.token, "--set", "approved=true"]);
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);

// ── the queue: filters and bounds ───────────────────────────────────────────────

test("external-tasks --process / --version / --task — narrow the queue", async () => {
  const { name, id } = park("etfilter");
  await waitForExternalToken(id);

  expect(queue(["--process", name]).length).toBe(1);
  expect(queue(["--process", uid("other")])).toEqual([]);
  expect(queue(["--process", name, "--task", "approval"]).length).toBe(1);
  expect(queue(["--process", name, "--task", "nope"])).toEqual([]);
  // v1 is what apply created; version 0 means "any", so it is not a filter at all.
  expect(queue(["--process", name, "--version", "1"]).length).toBe(1);
  expect(queue(["--process", name, "--version", "2"])).toEqual([]);

  runCli(bin, ["resolve", await waitForExternalToken(id), "--set", "approved=true"]);
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);

test("external-tasks --since / --until — bound the park time", async () => {
  const { name, id } = park("etwindow");
  await waitForExternalToken(id);

  expect(queue(["--process", name, "--since", "1h"]).length).toBe(1);
  expect(queue(["--process", name, "--since", "1h", "--until", "2000-01-01"])).toEqual([]);

  const bad = runCli(bin, ["external-tasks", "--since", "30"]);
  expect(bad.ok).toBe(false);
  expect(bad.stderr).toContain("invalid --since");

  runCli(bin, ["resolve", await waitForExternalToken(id), "--set", "approved=true"]);
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);

test("external-tasks — oldest→newest, so the newest waiting task prints last", async () => {
  const name = uid("etorder");
  runCli(bin, ["apply", "-f", writeDefs([externalDef(name)])]);

  // Park in order, waiting for each to enqueue so their waiting_since is deterministic.
  const ids: string[] = [];
  for (let i = 0; i < 3; i++) {
    const id = startedID(runCli(bin, ["run", name]).stdout);
    await waitForExternalToken(id);
    ids.push(id);
  }

  const posOf = (toks: string[], id: string) => toks.findIndex((t) => t.startsWith(`${id}.`));
  const all = queue(["--process", name]).map((t) => t.token);
  expect(all.length).toBe(3);
  expect(posOf(all, ids[0])).toBeLessThan(posOf(all, ids[2]));

  // --since streams forward instead of reversing a descending fetch; both must agree.
  expect(queue(["--process", name, "--since", "1h"]).map((t) => t.token)).toEqual(all);

  for (const id of ids) {
    runCli(bin, ["resolve", await waitForExternalToken(id), "--set", "approved=true"]);
    expect(await waitForInstance(id)).toBe("completed");
  }
}, 40_000);

test("external-tasks — an empty queue says so, and --json prints []", () => {
  const nothing = uid("nothing_parked");
  const r = runCli(bin, ["external-tasks", "--process", nothing]);
  expect(r.ok).toBe(true);
  expect(r.stdout.trim()).toBe("no external tasks waiting");
  expect(r.stderr).toBe("");
  expect(runCli(bin, ["external-tasks", "--process", nothing, "--json"]).stdout.trim()).toBe("[]");
});

// ── resolve ─────────────────────────────────────────────────────────────────────

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
  const { name, id } = park("resolve_bad");
  const token = await waitForExternalToken(id);

  const r = runCli(bin, ["resolve", token, "--set", "approved=notabool"]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("result is not valid for this task");
  // Rejected, not consumed: the token still resolves.
  expect(queue(["--process", name]).length).toBe(1);

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
