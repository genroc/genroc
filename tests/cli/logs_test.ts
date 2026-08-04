import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { waitForInstance } from "../helpers/client.ts";
import {
  BIG_BLOB,
  blobInputDef,
  childDef,
  failingDef,
  localDate,
  missingID,
  startedID,
  switchDef,
  uid,
} from "../helpers/genctl.ts";

// The log entity: `genctl logs`. One command, but the widest flag surface in the CLI —
// three output modes, two time columns, a level filter, a subtree switch, payload
// resolution and the shared windowing flags. Each is pinned here, along with the columns
// and separators the rendering commits to.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

const logTailDefault = 200; // cmd/genctl/commands.go

/** Apply a definition, run it to completion, and return the instance id. */
async function ran(def: object & { name: string }, want = "completed"): Promise<string> {
  runCli(bin, ["apply", "-f", writeDefs([def])]);
  const id = startedID(runCli(bin, ["run", def.name]).stdout);
  expect(await waitForInstance(id)).toBe(want);
  return id;
}

/**
 * Log rows as the server's JSON objects, in the order genctl prints them. --mode json
 * forwards each row verbatim, so the field names are LogEntryResp's — `instance`, not the
 * `id` the column layout abbreviates it to.
 */
function jsonRows(id: string, extra: string[] = [], env: Record<string, string> = {}) {
  return runCli(bin, ["logs", id, "--mode", "json", ...extra], env)
    .stdout.trim()
    .split("\n")
    .filter(Boolean)
    .map(
      (l) =>
        JSON.parse(l) as {
          time: string;
          instance: string;
          depth: number;
          level: string;
          event: string;
        },
    );
}

// ── columns and ordering ────────────────────────────────────────────────────────

test("logs — the table commits to TIME, LEVEL, EVENT and TASK", async () => {
  const id = await ran(switchDef(uid("cols")));
  const lines = runCli(bin, ["logs", id]).stdout.trim().split("\n");

  expect(lines[0].split(/\s+/)).toEqual(["TIME", "LEVEL", "EVENT", "TASK"]);
  // The ID column appears only under --recursive; a single-instance view repeats one id.
  expect(lines[0]).not.toContain("ID");
});

test("logs — entries print oldest→newest, with the newest nearest the prompt", async () => {
  const id = await ran(switchDef(uid("order")));
  const times = jsonRows(id).map((r) => new Date(r.time).getTime());

  expect(times.length).toBeGreaterThanOrEqual(2);
  expect(times).toEqual([...times].sort((a, b) => a - b));
  // The first event of any trail is the instance being created.
  expect(jsonRows(id)[0].event).toBe("inst_created");
});

// ── modes ───────────────────────────────────────────────────────────────────────

test("logs --mode — detail carries the data body, basic drops it, json is one object per line", async () => {
  const id = await ran(switchDef(uid("modes")));

  const detail = runCli(bin, ["logs", id, "--mode", "detail"]).stdout;
  const basic = runCli(bin, ["logs", id, "--mode", "basic"]).stdout;
  // Same rows through the same columns; only the trailing body differs.
  expect(basic.split("\n").length).toBe(detail.split("\n").length);
  expect(basic.length).toBeLessThanOrEqual(detail.length);
  expect(basic).not.toContain("output=");

  // json is JSONL, not a {items,page} array — every line parses on its own.
  const rows = jsonRows(id);
  expect(rows.length).toBeGreaterThan(0);
  for (const r of rows) expect(typeof r.event).toBe("string");

  const bad = runCli(bin, ["logs", id, "--mode", "verbose"]);
  expect(bad.ok).toBe(false);
  expect(bad.stderr).toContain("invalid log mode");
});

test("logs --mode json — timestamps are UTC RFC3339, independent of the reader's zone", async () => {
  const id = await ran(switchDef(uid("jsontz")));
  const prague = jsonRows(id, [], { TZ: "Europe/Prague" }).map((r) => r.time);
  const utc = jsonRows(id, [], { TZ: "UTC" }).map((r) => r.time);

  // The machine form never depends on who ran the command, unlike the column views.
  expect(prague).toEqual(utc);
  for (const t of utc) expect(t.endsWith("Z")).toBe(true);
});

// ── the time column ─────────────────────────────────────────────────────────────

test("logs --time clock — a day separator carries the date and zone offset", async () => {
  const id = await ran(switchDef(uid("datebreak")));
  const lines = runCli(bin, ["logs", id]).stdout.trim().split("\n");

  const breaks = lines.filter((l) => l.startsWith("--- "));
  // One run finishes within a day, so exactly one break — emitted even though every row
  // is from today, since the clock column carries no date of its own.
  expect(breaks.length).toBe(1);
  expect(breaks[0]).toMatch(new RegExp(`^--- ${localDate()} [+-]\\d{2}:\\d{2} ---$`));
  expect(lines[1]).toBe(breaks[0]); // directly under the header

  // The zone is an offset, never an abbreviation: "CST" is two zones 14 hours apart.
  expect(breaks[0]).not.toMatch(/[A-Z]{3,4} ---$/);
});

test("logs --time full — the date moves into the column and the separators go away", async () => {
  const id = await ran(switchDef(uid("timefull")));
  const lines = runCli(bin, ["logs", id, "--time", "full"]).stdout.trim().split("\n");

  // Exactly one place carries the date.
  expect(lines.filter((l) => l.startsWith("--- "))).toEqual([]);
  for (const l of lines.slice(1)) {
    expect(l).toMatch(new RegExp(`^${localDate()} \\d{2}:\\d{2}:\\d{2} [+-]\\d{2}:\\d{2}  `));
  }
  // The widened column shifts every later column with it, header included.
  expect(lines[0].indexOf("LEVEL")).toBe(lines[1].indexOf("INFO"));

  const bad = runCli(bin, ["logs", id, "--time", "iso"]);
  expect(bad.ok).toBe(false);
  expect(bad.stderr).toContain("invalid time style");
});

test("logs — $TZ moves the rendered times and the window flags together", async () => {
  const id = await ran(switchDef(uid("tz")));
  const utc = runCli(bin, ["logs", id, "--time", "full"], { TZ: "UTC" }).stdout.trim().split("\n");
  expect(utc[1]).toMatch(/ \+00:00 {2}/);

  // The round trip that matters: a timestamp read off a row, passed back as --since under
  // the same TZ, still selects that row.
  const stamp = utc[1].slice(0, 16);
  const back = runCli(bin, ["logs", id, "--time", "full", "--since", stamp], { TZ: "UTC" });
  expect(back.stdout).toContain(utc[1]);
});

// ── filters ─────────────────────────────────────────────────────────────────────

test("logs --level — narrows to one level", async () => {
  // A failing fetch is what spans levels: debug for the attempt, info for the lifecycle
  // rows, error for the failure. A completing process is info-only.
  const id = await ran(failingDef(uid("level")), "failed");

  const levels = (extra: string[] = []) => jsonRows(id, extra).map((r) => r.level);
  const all = levels();
  expect(new Set(all).size).toBeGreaterThan(1);

  for (const level of ["error", "info", "debug"]) {
    const only = levels(["--level", level]);
    expect(only.length).toBeGreaterThan(0);
    expect(only.every((l) => l === level)).toBe(true);
    expect(only.length).toBeLessThan(all.length);
  }

  // A level outside the enum is not rejected by either side, so it filters to nothing.
  // Documenting current behaviour: a typo'd --level reads as "no rows", not as an error.
  const unknown = runCli(bin, ["logs", id, "--level", "critical"]);
  expect(unknown.ok).toBe(true);
  expect(unknown.stdout.trim()).toBe("");
}, 15_000);

test("logs --recursive — adds the subtree's rows and the ID column that tells them apart", async () => {
  const child = uid("child");
  const parent = uid("parent");
  runCli(bin, ["apply", "-f", writeDefs([switchDef(child), childDef(parent, child)])]);
  const id = startedID(runCli(bin, ["run", parent]).stdout);
  expect(await waitForInstance(id)).toBe("completed");

  const flat = jsonRows(id);
  const tree = jsonRows(id, ["--recursive"]);
  expect(tree.length).toBeGreaterThan(flat.length);
  // The subtree spans more than one instance, which is why the id becomes a column.
  expect(new Set(tree.map((r) => r.instance)).size).toBeGreaterThan(1);
  // depth is the distance from the queried root, so the child's rows sit below it.
  expect(tree.some((r) => r.depth > 0)).toBe(true);
  expect(flat.every((r) => r.depth === 0)).toBe(true);

  const header = runCli(bin, ["logs", id, "--recursive"]).stdout.trim().split("\n")[0];
  expect(header.split(/\s+/)).toEqual(["TIME", "LEVEL", "ID", "EVENT", "TASK"]);
}, 15_000);

// ── window and cap ──────────────────────────────────────────────────────────────

test("logs --since / --until — bound the trail, half-open, and reject a bare integer", async () => {
  const id = await ran(switchDef(uid("window")));
  const times = (extra: string[], env: Record<string, string> = {}) =>
    jsonRows(id, extra, env).map((r) => new Date(r.time).getTime());

  const all = times(["--since", "1h"]);
  expect(all.length).toBeGreaterThanOrEqual(3);
  expect(times(["--since", localDate(new Date(Date.now() + 86_400_000))])).toEqual([]);

  // [since, until) is half-open, so adjacent windows partition without repeating a row.
  const cut = new Date(all[1]).toISOString().slice(0, 19).replace("T", " ");
  const before = times(["--since", "1h", "--until", cut], { TZ: "UTC" });
  const after = times(["--since", cut], { TZ: "UTC" });
  expect(before.every((t) => t < new Date(`${cut}Z`).getTime())).toBe(true);
  expect(before.length + after.length).toBe(times(["--since", "1h"], { TZ: "UTC" }).length);

  for (const flag of ["--since", "--until"]) {
    const bad = runCli(bin, ["logs", id, flag, "30"]);
    expect(bad.ok).toBe(false);
    expect(bad.stderr).toContain(`invalid ${flag}`);
  }
});

test("logs — the cap is a fixed default, not a flag; --since is the way past it", async () => {
  const id = await ran(switchDef(uid("nolimit")));

  // A trail under the cap reads the same either way, and never claims truncation.
  const bare = runCli(bin, ["logs", id, "--mode", "json"]);
  expect(bare.stderr).toBe("");
  expect(jsonRows(id).length).toBe(jsonRows(id, ["--since", "1h"]).length);

  // --limit is gone; the flag error is what points at --since in its place.
  const stale = runCli(bin, ["logs", id, "--limit", "2"]);
  expect(stale.ok).toBe(false);
  expect(stale.stderr).toContain("not defined: -limit");
  expect(stale.stderr).toContain("-since");
  expect(logTailDefault).toBeGreaterThan(0);
});

// ── payloads ────────────────────────────────────────────────────────────────────

test("logs — an externalized payload shows its {ref, size} reference until --resolve", async () => {
  // Past the inline threshold the payload lives in the object store, so the trail carries
  // a reference rather than the bytes. Exercised through the compiled binary, so the
  // data_ref → body rendering is covered and not just the HTTP layer.
  const name = uid("biglogs");
  runCli(bin, ["apply", "-f", writeDefs([blobInputDef(name)])]);
  const id = runCli(bin, ["run", name, "--input", JSON.stringify({ blob: BIG_BLOB }), "-q"])
    .stdout.trim();
  expect(await waitForInstance(id)).toBe("completed");

  const plain = runCli(bin, ["logs", id]);
  expect(plain.ok).toBe(true);
  expect(plain.stdout).toMatch(/input=\{"ref":"[0-9a-f]{32}","size":\d+\}/);
  expect(plain.stdout).not.toContain("BBBBBBBBBB");

  const resolved = runCli(bin, ["logs", id, "--resolve"]);
  expect(resolved.ok).toBe(true);
  expect(resolved.stdout).toContain("BBBBBBBBBB");
  // The value replaces the reference rather than sitting beside it.
  expect(resolved.stdout).not.toMatch(/"ref":"[0-9a-f]{32}"/);
}, 15_000);

// ── missing instance ────────────────────────────────────────────────────────────

test("logs — an id that does not exist is silently empty, unlike every other verb", () => {
  // Documenting a real gap: the listing filters on instance_id and finds nothing, so a
  // typo'd id is indistinguishable from an instance with no trail yet. get/pause/resume/
  // retry all 404 on the same id.
  const r = runCli(bin, ["logs", missingID]);
  expect(r.exitCode).toBe(0);
  expect(r.stdout).toBe("");
  expect(r.stderr).toBe("");
});
