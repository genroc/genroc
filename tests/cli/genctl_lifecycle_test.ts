import { mkdtempSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { client, waitForInstance } from "../helpers/client.ts";

// Covers the genctl commands the rest of the CLI suite skips: the lifecycle verbs
// (pause / resume / retry) and config, plus a regression guard on `status`'s stale-ref
// ordering (FindStaleRefs was unordered, so its rows came back in arbitrary DB order).

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000); // first build on a cold CI cache can exceed the 10s default

function uid(prefix: string) {
  return `${prefix}_${crypto.randomUUID().replace(/-/g, "").slice(0, 8)}`;
}

function startedID(stdout: string): string {
  const m = stdout.match(/started:\s+(\S+)/);
  if (!m) throw new Error(`no started id in: ${stdout}`);
  return m[1];
}

// A process that completes immediately (one switch task straight to end).
function switchDef(name: string) {
  return { name, tasks: [{ id: "s1", switch: [{ goto: "end" }] }] };
}

// A process whose task parks on an external action awaiting an {approved: boolean}
// result, so it sits `running external` — a stable, non-terminal state to pause.
function externalDef(name: string) {
  return {
    name,
    tasks: [
      {
        id: "approval",
        action: {
          type: "external",
          input: { msg: "approve me" },
          result_schema: {
            type: "object",
            properties: { approved: { type: "boolean" } },
            required: ["approved"],
          },
        },
        output: "$: self.result",
        switch: [{ goto: "end" }],
      },
    ],
  };
}

// A process whose only task calls an unreachable endpoint. With no on_error rules a
// call error is unhandled, so the instance fails on the first attempt (no retry).
function failingDef(name: string) {
  return {
    name,
    tasks: [
      {
        id: "call",
        action: { type: "fetch", url: "http://127.0.0.1:1/x" },
        timeout: 1000,
        switch: [{ goto: "end" }],
      },
    ],
  };
}

// A parent that fans out to two children under one task (child_map), so both share the
// same task_id and differ only by child name — the case that exercises FindStaleRefs's
// secondary ORDER BY key (child_name).
function twoChildDef(name: string, childA: string, childB: string) {
  return {
    name,
    tasks: [
      {
        id: "spawn",
        action: {
          type: "child_map",
          children: { a: { name: childA }, b: { name: childB } },
        },
        switch: [{ goto: "end" }],
      },
    ],
  };
}

async function waitForExternalToken(
  id: string,
  timeoutMs = 5000,
): Promise<string> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const { data, error } = await client.GET("/external-tasks", {});
    if (error)
      throw new Error(`list external tasks failed: ${JSON.stringify(error)}`);
    const entry = (data?.items ?? []).find(
      (t) => typeof t.token === "string" && t.token.startsWith(`${id}.`),
    );
    if (entry?.token) return entry.token;
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(
    `external task for ${id} was not queued within ${timeoutMs}ms`,
  );
}

// ── pause / resume ──────────────────────────────────────────────────────────────

test("pause then resume — parks an external instance paused and revives it", async () => {
  const name = uid("pausable");
  runCli(bin, ["apply", "-f", writeDefs([externalDef(name)])]);
  const id = startedID(runCli(bin, ["run", name]).stdout);
  await waitForExternalToken(id); // ensure it has parked (not leased) before pausing

  const p = runCli(bin, ["pause", id]);
  expect(p.ok).toBe(true);
  expect(p.stdout).toContain(`paused: ${id}`);

  // The instance reports paused; pause is not a terminal outcome, so it just stops advancing.
  const g = runCli(bin, ["get", id]);
  expect(g.ok).toBe(true);
  expect(g.stdout).toContain("paused");

  const r = runCli(bin, ["resume", id]);
  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(`resumed: ${id}`);

  // Clean up so the instance doesn't linger parked on the shared server: re-arm the
  // task (it re-queues a poll after resume), then resolve it to completion.
  const token = await waitForExternalToken(id);
  runCli(bin, ["resolve", token, "--set", "approved=true"]);
  expect(await waitForInstance(id)).toBe("completed");
});

// ── retry ───────────────────────────────────────────────────────────────────────

test("retry — re-arms a failed instance (plain and --force)", async () => {
  const name = uid("failing");
  runCli(bin, ["apply", "-f", writeDefs([failingDef(name)])]);

  const id1 = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id1, 15_000)).toBe("failed");
  const r1 = runCli(bin, ["retry", id1]);
  expect(r1.ok).toBe(true);
  expect(r1.stdout).toContain(`retried: ${id1}`);

  const id2 = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id2, 15_000)).toBe("failed");
  const r2 = runCli(bin, ["retry", "--force", id2]);
  expect(r2.ok).toBe(true);
  expect(r2.stdout).toContain(`retried: ${id2}`);
}, 40_000);

test("retry — rejected on an instance that has not failed", async () => {
  const name = uid("done");
  runCli(bin, ["apply", "-f", writeDefs([switchDef(name)])]);
  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("completed");

  const r = runCli(bin, ["retry", id]);
  expect(r.ok).toBe(false);
  expect(r.exitCode).not.toBe(0);
  expect(r.stderr).toContain("genctl:"); // routed through fatal()
});

// ── config ────────────────────────────────────────────────────────────────────

// Isolate the config file per test via HOME/XDG (os.UserConfigDir reads $HOME on
// macOS, $XDG_CONFIG_HOME on Linux — set both so it lands in the temp dir either way).
function configEnv() {
  const home = mkdtempSync(join(tmpdir(), "genctl_cfg_"));
  return { HOME: home, XDG_CONFIG_HOME: join(home, ".config") };
}

test("config set then get — server round-trips through the config file", () => {
  const env = configEnv();
  const url = "http://config.example.test:9999";

  const s = runCli(bin, ["config", "set", "server", url], env);
  expect(s.ok).toBe(true);
  expect(s.stdout).toContain(`set server = ${url}`);

  const g = runCli(bin, ["config", "get", "server"], env);
  expect(g.ok).toBe(true);
  expect(g.stdout.trim()).toBe(url);
});

test("config get — unset server prints (not set); unknown key errors", () => {
  const env = configEnv();

  const g = runCli(bin, ["config", "get", "server"], env);
  expect(g.ok).toBe(true);
  expect(g.stdout).toContain("(not set)");

  const bad = runCli(bin, ["config", "get", "bogus"], env);
  expect(bad.ok).toBe(false);
  expect(bad.stderr).toContain("unknown config key");
});

// ── newest-at-bottom display + --limit tail window ──────────────────────────────

// A three-task process — enough ordered log entries to exercise the --limit tail.
function threeTaskDef(name: string) {
  return {
    name,
    tasks: [
      { id: "s1", switch: [{ goto: "next" }] },
      { id: "s2", switch: [{ goto: "next" }] },
      { id: "s3", switch: [{ goto: "end" }] },
    ],
  };
}

// Fetch log-entry timestamps (ms) in the order genctl prints them (top → bottom).
function logTimes(id: string, extra: string[] = []): number[] {
  return runCli(bin, ["logs", id, "--mode", "json", ...extra])
    .stdout.trim()
    .split("\n")
    .filter(Boolean)
    .map((l) => new Date(JSON.parse(l).time).getTime());
}

test("logs — entries print oldest→newest, newest at the bottom", async () => {
  const name = uid("ordered");
  runCli(bin, ["apply", "-f", writeDefs([threeTaskDef(name)])]);
  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("completed");

  const times = logTimes(id);
  expect(times.length).toBeGreaterThanOrEqual(2);
  // Chronological (non-decreasing) top→bottom: the most recent line is last.
  expect(times).toEqual([...times].sort((a, b) => a - b));
});

test("logs --limit — keeps the newest N, still oldest→newest (tail)", async () => {
  const name = uid("tail");
  runCli(bin, ["apply", "-f", writeDefs([threeTaskDef(name)])]);
  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("completed");

  const all = logTimes(id, ["--limit", "100"]);
  expect(all.length).toBeGreaterThanOrEqual(3);

  const tail2 = logTimes(id, ["--limit", "2"]);
  expect(tail2.length).toBe(2);
  // --limit keeps the two most-recent entries, displayed chronologically — i.e. the
  // last two of the full list. This is the whole point: a limit is a tail, not a head.
  expect(tail2).toEqual(all.slice(-2));
});

test("instances — the newest run prints last (oldest→newest)", async () => {
  const name = uid("ord_inst");
  runCli(bin, ["apply", "-f", writeDefs([switchDef(name)])]);
  const ids = [0, 1, 2].map(() => startedID(runCli(bin, ["run", name]).stdout));
  for (const id of ids) expect(await waitForInstance(id)).toBe("completed");

  // A generous limit so all three fall inside the recent window on the shared server.
  const arr = JSON.parse(
    runCli(bin, ["instances", "--json", "--limit", "200"]).stdout,
  ) as { id: string }[];
  const posOf = (id: string) => arr.findIndex((it) => it.id === id);

  expect(posOf(ids[0])).toBeGreaterThanOrEqual(0);
  expect(posOf(ids[2])).toBeGreaterThanOrEqual(0);
  // Created oldest→newest as ids[0],ids[1],ids[2]; displayed oldest→newest, so the
  // first-created appears above the last-created (newest nearest the prompt).
  expect(posOf(ids[0])).toBeLessThan(posOf(ids[2]));
});

test("external-tasks — newest waiting task prints last; --limit keeps the newest", async () => {
  const name = uid("etorder");
  runCli(bin, ["apply", "-f", writeDefs([externalDef(name)])]);

  // Park three tasks in order, waiting for each to enqueue before starting the next so
  // their waiting_since (and thus their order) is deterministic.
  const ids: string[] = [];
  for (let i = 0; i < 3; i++) {
    const id = startedID(runCli(bin, ["run", name]).stdout);
    await waitForExternalToken(id);
    ids.push(id);
  }

  // Filter to this process so other files' parked tasks don't crowd the page. A token
  // is "<instance-id>.<nonce>", so its instance is identifiable by prefix.
  const tokens = (extra: string[] = []): string[] =>
    (
      JSON.parse(
        runCli(bin, ["external-tasks", "--process", name, "--json", ...extra]).stdout,
      ) as { token: string }[]
    ).map((t) => t.token);
  const posOf = (toks: string[], id: string) =>
    toks.findIndex((tok) => tok.startsWith(`${id}.`));

  // Full list: oldest→newest, so the first-parked is above the last-parked.
  const all = tokens();
  expect(all.length).toBe(3);
  expect(posOf(all, ids[0])).toBeLessThan(posOf(all, ids[2]));

  // --limit keeps the newest N (tail): the oldest task drops out, order preserved.
  const two = tokens(["--limit", "2"]);
  expect(two.length).toBe(2);
  expect(posOf(two, ids[0])).toBe(-1); // oldest dropped
  expect(posOf(two, ids[1])).toBeGreaterThanOrEqual(0);
  expect(posOf(two, ids[2])).toBeGreaterThanOrEqual(0);
  expect(posOf(two, ids[1])).toBeLessThan(posOf(two, ids[2]));

  // Resolve all three so they don't linger parked on the shared server.
  for (const id of ids) {
    const token = await waitForExternalToken(id);
    runCli(bin, ["resolve", token, "--set", "approved=true"]);
    expect(await waitForInstance(id)).toBe("completed");
  }
}, 30_000);

// ── status stale-ref ordering (regression for unordered FindStaleRefs) ───────────

test("status — stale refs are ordered deterministically by child name", () => {
  // Names chosen so the alphabetical order (aaa_ before zzz_) is independent of the
  // random suffix and of the child_map key order the server iterates.
  const childA = uid("aaa_child");
  const childB = uid("zzz_child");
  const parent = uid("parent");
  const track = uid("track");

  runCli(bin, [
    "apply",
    "-f",
    writeDefs([
      switchDef(childA),
      switchDef(childB),
      twoChildDef(parent, childA, childB),
    ]),
    "--channel",
    track,
  ]);

  // Advance both children past the version the parent baked, making both refs stale.
  const bump = (n: string) => ({
    ...switchDef(n),
    tasks: [{ id: "s2", switch: [{ goto: "end" }] }],
  });
  runCli(bin, [
    "apply",
    "-f",
    writeDefs([bump(childA), bump(childB)]),
    "--channel",
    track,
  ]);

  const r = runCli(bin, ["status", "--channel", track]);
  expect(r.ok).toBe(true);
  expect(r.stdout).toContain("STALE");
  expect(r.stdout).toContain(childA);
  expect(r.stdout).toContain(childB);

  // Both refs hang off the same parent task, differing only by child name: the
  // alphabetically-first child must be listed first (the FindStaleRefs ORDER BY).
  expect(r.stdout.indexOf(childA)).toBeLessThan(r.stdout.indexOf(childB));

  // And the order is stable across repeated calls (no run-to-run reshuffling).
  const again = runCli(bin, ["status", "--channel", track]);
  expect(again.stdout).toBe(r.stdout);
});
