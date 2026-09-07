import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { API_BASE, waitForInstance } from "../helpers/client.ts";
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
const defaultLogWidth = 120; // cmd/genctl/format.go

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
          level: string;
          event: string;
          actor?: string;
        },
    );
}

// ── columns and ordering ────────────────────────────────────────────────────────

test("logs — the table commits to TIME, LEVEL, EVENT and TASK", async () => {
  const id = await ran(switchDef(uid("cols")));
  const lines = runCli(bin, ["logs", id]).stdout.trim().split("\n");

  expect(lines[0].split(/\s+/)).toEqual(["TIME", "LEVEL", "ID", "EVENT", "TASK"]);
  // --flat is one instance's own rows, where an ID column would repeat one id on every line.
  const own = runCli(bin, ["logs", id, "--flat"]).stdout.trim().split("\n");
  expect(own[0].split(/\s+/)).toEqual(["TIME", "LEVEL", "EVENT", "TASK"]);
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

test("logs — the default floor is info, and debug is where the engine's own rows are", async () => {
  const id = await ran(failingDef(uid("dflt")), "failed");

  const bare = jsonRows(id);
  expect(bare.length).toBeGreaterThan(0);
  expect(
    bare.map((r) => r.level),
    "a bare trail carried a debug row; the default is a floor at info, so an operator reading "
      + "a run is not handed the engine's per-advance bookkeeping unasked",
  ).not.toContain("debug");
  // What the run DID is above the floor: the call that failed and the end it came to.
  expect(bare.map((r) => r.event)).toContain("action_failed");
  expect(bare.map((r) => r.event)).toContain("inst_failed");
  expect(bare.map((r) => r.event)).not.toContain("work_started");

  // ...and nothing is lost, only unasked for: debug is the bottom of the floor.
  const full = jsonRows(id, ["--level", "debug"]);
  expect(full.length).toBeGreaterThan(bare.length);
  expect(full.map((r) => r.event)).toContain("work_started");
}, 15_000);

// ── attribution and line width ──────────────────────────────────────────────────

test("logs — an operator's attribution prints, the engine's own does not", async () => {
  const id = await ran(switchDef(uid("actor")));
  const out = runCli(bin, ["logs", id], { COLUMNS: "500" }).stdout;

  expect(
    out,
    "engine:self is on nearly every row, so printing by= there spends a field per line to say nothing",
  ).not.toContain("by=engine:self");
  // The row a person caused still names them: `run` reached the API as the operator.
  expect(out).toMatch(/inst_created\s+by=\S+/);
  // A render choice, not a write one -- the stored trail is what an audit answers with.
  expect(
    jsonRows(id).some((r) => r.actor === "engine:self"),
    "the engine stopped recording its own attribution; empty means \"predates migration 038\"",
  ).toBe(true);
});

test("logs — a long payload is cut to the line width, never wrapped", async () => {
  const name = uid("wide");
  runCli(bin, ["apply", "-f", writeDefs([blobInputDef(name)])]);
  // Under the 2 KiB object-store cutoff, so the payload stays in the row rather than
  // becoming a ref -- a long line is the thing under test.
  const blob = "W".repeat(400);
  const id = runCli(bin, ["run", name, "--input", JSON.stringify({ blob }), "-q"]).stdout.trim();
  expect(await waitForInstance(id)).toBe("completed");

  const narrow = runCli(bin, ["logs", id], { COLUMNS: "80" }).stdout.trim().split("\n");
  for (const l of narrow) {
    expect(
      l.length,
      `a row wider than the terminal wraps, and takes the column alignment of every row after it with it: ${l}`,
    ).toBeLessThanOrEqual(80);
  }
  expect(narrow.some((l) => l.endsWith("…"))).toBe(true);

  // A wider budget shows more of the same row, and json is the form that is never cut.
  const wide = runCli(bin, ["logs", id], { COLUMNS: "200" }).stdout.trim().split("\n");
  expect(wide.length).toBe(narrow.length);
  expect(wide.join("").length).toBeGreaterThan(narrow.join("").length);
  expect(runCli(bin, ["logs", id, "--mode", "json"]).stdout).toContain(blob);

  // No COLUMNS: a fixed default width, not the whole line.
  const dflt = runCli(bin, ["logs", id], { COLUMNS: "" }).stdout.trim().split("\n");
  expect(Math.max(...dflt.map((l) => l.length))).toBeLessThanOrEqual(defaultLogWidth);
  expect(dflt.some((l) => l.endsWith("…"))).toBe(true);
}, 15_000);

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

test("logs --level — is a floor, keeping every level above it", async () => {
  // A failing fetch is what spans levels: debug for the attempt, info for the lifecycle
  // rows, error for the failure. A completing process is info-only.
  const id = await ran(failingDef(uid("level")), "failed");

  const levels = (extra: string[] = []) => jsonRows(id, extra).map((r) => r.level);
  // debug is the bottom, so it is the whole trail -- the default is info.
  const all = levels(["--level", "debug"]);
  expect(new Set(all)).toContain("error");
  expect(new Set(all)).toContain("debug");

  // The floor that matters: asking for info must not hide the failure above it.
  const fromInfo = levels(["--level", "info"]);
  expect(
    fromInfo,
    "an error was filtered out by --level info -- a level filter that hides the errors above "
      + "it answers \"is anything wrong here?\" with silence",
  ).toContain("error");
  expect(fromInfo).not.toContain("debug");

  // error is the top, so it is only errors.
  expect(levels(["--level", "error"]).every((l) => l === "error")).toBe(true);
  expect(levels(["--level", "warn"]).every((l) => l === "warn" || l === "error")).toBe(true);

  // A level outside the vocabulary has no set above it, so it is refused rather than
  // filtered to nothing -- an empty trail reads as "nothing happened".
  const unknown = runCli(bin, ["logs", id, "--level", "critical"]);
  expect(unknown.ok).toBe(false);
  expect(unknown.stderr).toContain("invalid --level");

  // ...and so does the server, reached directly: genctl's check is a fast answer, not the rule.
  const direct = await fetch(`${API_BASE}/instances/${id}/logs?level=critical`);
  expect(direct.status).toBe(400);
  expect(((await direct.json()) as { error: string }).error).toContain("debug, info, warn, error");
}, 15_000);

test("logs — a root's trail is its whole tree, and --flat is its own rows", async () => {
  const child = uid("child");
  const parent = uid("parent");
  runCli(bin, ["apply", "-f", writeDefs([switchDef(child), childDef(parent, child)])]);
  const id = startedID(runCli(bin, ["run", parent]).stdout);
  expect(await waitForInstance(id)).toBe("completed");

  const tree = jsonRows(id);
  const own = jsonRows(id, ["--flat"]);
  expect(tree.length).toBeGreaterThan(own.length);
  // The tree spans more than one instance, which is why the id becomes a column.
  expect(new Set(tree.map((r) => r.instance)).size).toBeGreaterThan(1);
  // --flat is this instance's own rows, so the root is the only id in them.
  expect(new Set(own.map((r) => r.instance))).toEqual(new Set([id]));

  const header = runCli(bin, ["logs", id]).stdout.trim().split("\n")[0];
  expect(header.split(/\s+/)).toEqual(["TIME", "LEVEL", "ID", "EVENT", "TASK"]);
}, 15_000);

// A tree is addressed by its ROOT, as it is for pause/resume/retry/upgrade -- so a child id
// is a question about that child, and answers with its own rows rather than its siblings'.
test("logs — a child id answers with that child's own rows", async () => {
  const child = uid("child");
  const parent = uid("parent");
  runCli(bin, ["apply", "-f", writeDefs([switchDef(child), childDef(parent, child)])]);
  const rootID = startedID(runCli(bin, ["run", parent]).stdout);
  expect(await waitForInstance(rootID)).toBe("completed");

  const kidID = JSON.parse(
    runCli(bin, ["instances", "--process", child, "--children", "--since", "1h", "--json"]).stdout,
  )[0].id;

  const kidRows = jsonRows(kidID);
  expect(kidRows.length).toBeGreaterThan(0);
  expect(
    new Set(kidRows.map((r) => r.instance)),
    "a child's trail is its own, not the tree it sits in",
  ).toEqual(new Set([kidID]));
  // ...and the tree read from the root still carries them.
  expect(jsonRows(rootID).some((r) => r.instance === kidID)).toBe(true);
}, 15_000);

// ── window and cap ──────────────────────────────────────────────────────────────

test("logs --since / --until — bound the trail, half-open, and reject a bare integer", async () => {
  const id = await ran(switchDef(uid("window")));
  // The whole trail, level floor out of the way: this is about the window, and a partition
  // needs more rows than the default view of a two-event process has.
  const times = (extra: string[], env: Record<string, string> = {}) =>
    jsonRows(id, ["--level", "debug", ...extra], env).map((r) => new Date(r.time).getTime());

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

test("logs — an externalized payload shows its ref, and `object` fetches it", async () => {
  // Past the inline threshold the payload lives in the object store, so the trail carries a
  // handle rather than the bytes. `logs` never resolves: a trail is scanned, not read, and these
  // payloads are large by definition — printing the ref and fetching the one that matters is
  // the whole point of the split. specs/object-store.md.
  const name = uid("biglogs");
  runCli(bin, ["apply", "-f", writeDefs([blobInputDef(name)])]);
  const id = runCli(bin, ["run", name, "--input", JSON.stringify({ blob: BIG_BLOB }), "-q"])
    .stdout.trim();
  expect(await waitForInstance(id)).toBe("completed");

  // A wide budget: the ref is the point of this row, and the default width would cut it.
  const plain = runCli(bin, ["logs", id], { COLUMNS: "500" });
  expect(plain.ok).toBe(true);
  // The handle sits where the value was cut from, inside the payload's own shape -- the entry
  // is not replaced by a ref, only the leaf that was too big to carry.
  expect(plain.stdout).toMatch(/input=\{"blob":\{"ref":"[0-9a-f]{32}","size":\d+\}\}/);
  expect(plain.stdout).not.toContain("BBBBBBBBBB");

  // The escape hatch: fetch the one payload by the ref the trail printed.
  const ref = plain.stdout.match(/"ref":"([0-9a-f]{32})"/)![1];
  const fetched = runCli(bin, ["object", ref]);
  expect(fetched.ok, fetched.stderr).toBe(true);
  expect(fetched.stdout).toContain("BBBBBBBBBB");
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
