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

// ── newest-at-bottom display + the --since read window ──────────────────────────

// A three-task process — enough ordered log entries to exercise the --since window.
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

// genctl renders and parses timestamps in the local zone, so a UTC-derived date is off
// by one for part of every day — build the expected date the same way genctl does.
function localDate(at = new Date()): string {
  const p = (n: number) => String(n).padStart(2, "0");
  return `${at.getFullYear()}-${p(at.getMonth() + 1)}-${p(at.getDate())}`;
}

// Fetch log-entry timestamps (ms) in the order genctl prints them (top → bottom). env
// lets a caller pin TZ, so a timestamp built in UTC can be passed back as --since/--until
// and mean the same instant genctl reads it as.
function logTimes(id: string, extra: string[] = [], env: Record<string, string> = {}): number[] {
  return runCli(bin, ["logs", id, "--mode", "json", ...extra], env)
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

test("logs — the cap is a default, not a flag; --since is the only reach-back control", async () => {
  const name = uid("nolimit");
  runCli(bin, ["apply", "-f", writeDefs([threeTaskDef(name)])]);
  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("completed");

  // A trail under the cap reads the same either way, and never claims it was truncated.
  const bare = runCli(bin, ["logs", id, "--mode", "json"]);
  expect(bare.stderr).not.toContain("--since");
  expect(logTimes(id)).toEqual(logTimes(id, ["--since", "1h"]));

  // --limit is gone: the flag error is what points at --since in its place.
  const stale = runCli(bin, ["logs", id, "--limit", "2"]);
  expect(stale.ok).toBe(false);
  expect(stale.stderr).toContain("not defined: -limit");
  expect(stale.stderr).toContain("-since");
});

test("logs --since — a duration and a date bound the window; the future is empty", async () => {
  const name = uid("since");
  runCli(bin, ["apply", "-f", writeDefs([threeTaskDef(name)])]);
  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("completed");

  const all = logTimes(id);
  expect(all.length).toBeGreaterThanOrEqual(3);
  // A window that contains the run returns it whole, whether given as a duration back
  // from now or as a calendar date.
  expect(logTimes(id, ["--since", "1h"])).toEqual(all);
  expect(logTimes(id, ["--since", localDate()])).toEqual(all);

  const tomorrow = localDate(new Date(Date.now() + 86_400_000));
  expect(logTimes(id, ["--since", tomorrow])).toEqual([]);
});

test("logs --until — bounds the far end; [since, until) is half-open", async () => {
  const name = uid("until");
  runCli(bin, ["apply", "-f", writeDefs([threeTaskDef(name)])]);
  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("completed");

  const all = logTimes(id, ["--since", "1h"]);
  expect(all.length).toBeGreaterThanOrEqual(3);

  // An --until at the second entry's instant excludes that entry: the far end is
  // exclusive, so walking hour by hour never counts a boundary row twice.
  const cut = new Date(all[1]).toISOString().slice(0, 19).replace("T", " ");
  const before = logTimes(id, ["--since", "1h", "--until", cut], { TZ: "UTC" });
  const cutMs = new Date(`${cut}Z`).getTime();
  expect(before.every((t) => t < cutMs)).toBe(true);

  // The window's two halves partition the whole, with no row lost or repeated.
  const after = logTimes(id, ["--since", cut], { TZ: "UTC" });
  expect(before.length + after.length).toBe(logTimes(id, ["--since", "1h"], { TZ: "UTC" }).length);

  // A window that closes before anything happened is empty.
  expect(logTimes(id, ["--since", "1h", "--until", "2000-01-01"])).toEqual([]);
});

test("logs --since — a bare integer is rejected, not read as epoch millis", () => {
  // A concrete (nonexistent) id: --since is parsed before any request, so the id never
  // reaches the server and the test does not depend on @last being recorded.
  const r = runCli(bin, ["logs", "00000000-0000-0000-0000-000000000000", "--since", "30"]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("invalid --since");
});

test("logs — a date break precedes the first row of each day", async () => {
  const name = uid("datebreak");
  runCli(bin, ["apply", "-f", writeDefs([threeTaskDef(name)])]);
  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("completed");

  const lines = runCli(bin, ["logs", id]).stdout.trim().split("\n");
  const breaks = lines.filter((l) => l.startsWith("--- "));
  // One run finishes within a day, so exactly one break — emitted even though every
  // entry is from today, since the columns carry no date of their own. The zone is a
  // numeric offset, never an abbreviation ("CST" is two zones fourteen hours apart).
  expect(breaks.length).toBe(1);
  expect(breaks[0]).toMatch(new RegExp(`^--- ${localDate()} [+-]\\d{2}:\\d{2} ---$`));
  expect(lines[0]).toContain("TIME");
  expect(lines[1]).toBe(breaks[0]);
});

test("logs — $TZ moves both the rendered times and --since together", async () => {
  const name = uid("tz");
  runCli(bin, ["apply", "-f", writeDefs([threeTaskDef(name)])]);
  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("completed");

  const utc = runCli(bin, ["logs", id, "--time", "full"], { TZ: "UTC" }).stdout.trim().split("\n");
  expect(utc[1]).toMatch(/ \+00:00 {2}/);
  expect(utc.find((l) => l.startsWith("--- "))).toBeUndefined();

  // The round trip that matters: a timestamp read off a row, passed back as --since
  // under the same TZ, must still select that row.
  const stamp = utc[1].slice(0, 16); // "2026-08-04 09:43"
  const back = runCli(bin, ["logs", id, "--time", "full", "--since", stamp], { TZ: "UTC" });
  expect(back.stdout).toContain(utc[1]);
});

test("logs --time full — the date moves into the column and the separators go away", async () => {
  const name = uid("timefull");
  runCli(bin, ["apply", "-f", writeDefs([threeTaskDef(name)])]);
  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("completed");

  const lines = runCli(bin, ["logs", id, "--time", "full"]).stdout.trim().split("\n");
  // Exactly one place carries the date: with it in the column there are no separators.
  expect(lines.filter((l) => l.startsWith("--- "))).toEqual([]);
  // Every row is dated and carries its offset — a full timestamp with no zone is only
  // exact to whoever ran the command.
  for (const l of lines.slice(1)) {
    expect(l).toMatch(new RegExp(`^${localDate()} \\d{2}:\\d{2}:\\d{2} [+-]\\d{2}:\\d{2}  `));
  }
  // The widened time column shifts every later column with it, header included.
  expect(lines[0].indexOf("LEVEL")).toBe(lines[1].indexOf("INFO"));

  const bad = runCli(bin, ["logs", id, "--time", "iso"]);
  expect(bad.ok).toBe(false);
  expect(bad.stderr).toContain("invalid time style");
});

test("instances — the newest run prints last (oldest→newest)", async () => {
  const name = uid("ord_inst");
  runCli(bin, ["apply", "-f", writeDefs([switchDef(name)])]);
  const ids = [0, 1, 2].map(() => startedID(runCli(bin, ["run", name]).stdout));
  for (const id of ids) expect(await waitForInstance(id)).toBe("completed");

  // --since lifts the cap, so all three are present however busy the shared server is.
  // This is also the uncapped (forward-streaming) path: instances defaults to
  // newest-first server-side, so it is the one list that would come back reversed if the
  // walk inherited that default instead of asking for ascending.
  const arr = JSON.parse(
    runCli(bin, ["instances", "--json", "--since", "1h"]).stdout,
  ) as { id: string }[];
  const posOf = (id: string) => arr.findIndex((it) => it.id === id);

  expect(posOf(ids[0])).toBeGreaterThanOrEqual(0);
  expect(posOf(ids[2])).toBeGreaterThanOrEqual(0);
  // Created oldest→newest as ids[0],ids[1],ids[2]; displayed oldest→newest, so the
  // first-created appears above the last-created (newest nearest the prompt).
  expect(posOf(ids[0])).toBeLessThan(posOf(ids[2]));

  // The capped path must agree with the uncapped one on direction, not just on contents.
  const capped = JSON.parse(runCli(bin, ["instances", "--json"]).stdout) as {
    created_at: string;
  }[];
  const times = capped.map((i) => new Date(i.created_at).getTime());
  expect(times).toEqual([...times].sort((a, b) => a - b));
});

test("instances --since — bounds the column --sort selects, and rejects a bare integer", async () => {
  const name = uid("since_inst");
  runCli(bin, ["apply", "-f", writeDefs([switchDef(name)])]);
  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("completed");

  const idsIn = (extra: string[]) =>
    (JSON.parse(runCli(bin, ["instances", "--json", ...extra]).stdout) as { id: string }[])
      .map((i) => i.id);

  // Present under either sort key within the window, absent beyond it.
  expect(idsIn(["--since", "1h"])).toContain(id);
  expect(idsIn(["--since", "1h", "--sort", "updated"])).toContain(id);
  const tomorrow = localDate(new Date(Date.now() + 86_400_000));
  expect(idsIn(["--since", tomorrow])).toEqual([]);

  const bad = runCli(bin, ["instances", "--since", "30"]);
  expect(bad.ok).toBe(false);
  expect(bad.stderr).toContain("invalid --since");
});

test("definitions — newest registered first; --sort name gives the alphabetical order", () => {
  // Applied in one call, so both share a registration instant and only the name order is
  // predictable between them; zzz_ vs aaa_ is independent of the random suffix.
  const stem = uid("defs");
  const alpha = `${stem}_aaa`;
  const omega = `${stem}_zzz`;
  runCli(bin, ["apply", "-f", writeDefs([switchDef(omega), switchDef(alpha)])]);

  const names = (extra: string[] = []) =>
    (JSON.parse(runCli(bin, ["definitions", "--json", ...extra]).stdout) as { name: string }[])
      .map((d) => d.name);

  // --since lifts the cap, so both are reachable however many definitions the server holds.
  const recent = names(["--since", "1h"]);
  expect(recent).toContain(alpha);
  expect(recent).toContain(omega);

  // Same window under the name sort, now alphabetical rather than by registration — the
  // one list offering a non-time sort. --since lifts the cap here too, so the shared
  // server's other definitions cannot push these two off the page.
  const byName = names(["--since", "1h", "--sort", "name"]);
  expect(byName).toContain(alpha);
  expect(byName).toContain(omega);
  expect(byName.indexOf(alpha)).toBeLessThan(byName.indexOf(omega));
  expect(byName).toEqual([...byName].sort());

  // The table carries the registration time the default sort is keyed on.
  const table = runCli(bin, ["definitions", "--since", "1h"]).stdout.trim().split("\n");
  expect(table[0]).toContain("NAME");
  expect(table[0]).toContain("REGISTERED");

  const bad = runCli(bin, ["definitions", "--since", "30"]);
  expect(bad.ok).toBe(false);
  expect(bad.stderr).toContain("invalid --since");
});

test("external-tasks — newest waiting task prints last; --since lifts the cap", async () => {
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

  // --since lifts the cap and streams forward; same rows, same oldest→newest direction
  // as the capped path above, which reaches them by reversing a descending fetch.
  const within = tokens(["--since", "1h"]);
  expect(within).toEqual(all);
  // A window that starts after they parked is empty, not merely reordered.
  const tomorrow = localDate(new Date(Date.now() + 86_400_000));
  expect(tokens(["--since", tomorrow])).toEqual([]);

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
