import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { client } from "../helpers/client.ts";
import {
  childDef,
  frozenUntil,
  listCap,
  raisingDef,
  restDef,
  switchDef,
  uid,
} from "../helpers/genctl.ts";

// The definition entity: `apply` and `validate` that write it, and `definitions` that
// reads it back. Every flag of all three, plus the columns the table commits to.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

type DefRow = { name: string; version: number; created_at: string; raises?: string[] };

function defs(extra: string[] = []): DefRow[] {
  return JSON.parse(runCli(bin, ["definitions", "--json", ...extra]).stdout) as DefRow[];
}

// ── apply ───────────────────────────────────────────────────────────────────────

test("apply — reports saved for new content and unchanged for a re-apply", () => {
  const name = uid("proc");
  const file = writeDefs([switchDef(name)]);

  expect(runCli(bin, ["apply", "-f", file]).stdout).toContain(`saved: ${name}@v1`);
  // Content-addressed: identical bytes do not mint a v2, and the line says so rather
  // than silently printing "saved" again.
  expect(runCli(bin, ["apply", "-f", file]).stdout).toContain(`unchanged: ${name}@v1`);
});

test("apply — changed content mints the next version", () => {
  const name = uid("proc");
  runCli(bin, ["apply", "-f", writeDefs([switchDef(name)])]);
  const changed = { ...switchDef(name), tasks: [{ id: "s2", switch: [{ goto: "end" }] }] };

  expect(runCli(bin, ["apply", "-f", writeDefs([changed])]).stdout).toContain(`saved: ${name}@v2`);
  expect(defs(["--since", "1h"]).filter((d) => d.name === name).map((d) => d.version).sort()).toEqual([1, 2]);
});

test("apply -f — repeats to take several files, and each may hold several documents", () => {
  const [a, b, c] = [uid("a"), uid("b"), uid("c")];
  const r = runCli(bin, [
    "apply",
    "-f", writeDefs([switchDef(a), switchDef(b)]), // multi-document
    "-f", writeDefs([switchDef(c)]),               // second file
  ]);

  expect(r.ok).toBe(true);
  for (const n of [a, b, c]) expect(r.stdout).toContain(`saved: ${n}@v1`);
});

test("apply --channel — points the named channel at what was applied", async () => {
  const name = uid("proc");
  const channel = uid("ch");
  expect(runCli(bin, ["apply", "-f", writeDefs([switchDef(name)]), "--channel", channel]).ok).toBe(true);

  const { data } = await client.GET("/channels", { params: { query: { name } } });
  expect((data?.items ?? []).find((e) => e.channel === channel)?.version).toBe(1);
});

test("apply — without --channel the default channel is latest", async () => {
  const name = uid("proc");
  runCli(bin, ["apply", "-f", writeDefs([switchDef(name)])]);

  const { data } = await client.GET("/channels", { params: { query: { name } } });
  expect((data?.items ?? []).find((e) => e.channel === "latest")?.version).toBe(1);
});

test("apply — a self-referential process is accepted", () => {
  const name = uid("recursive");
  expect(runCli(bin, ["apply", "-f", writeDefs([childDef(name, name)])]).stdout).toContain(
    `saved: ${name}@v1`,
  );
});

test("apply — one invalid definition rolls the whole batch back", () => {
  const good = uid("good");
  // tasks must not be empty, so the second document is rejected.
  const r = runCli(bin, ["apply", "-f", writeDefs([switchDef(good), { name: uid("bad"), tasks: [] }])]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("genctl:");

  // A batch is one logical change: everything is validated before anything is written, so
  // a rejection anywhere leaves the registry exactly as it was. A partial apply would
  // leave parents pointing at children that were never stored.
  expect(defs(["--since", "1h"]).some((d) => d.name === good)).toBe(false);
  expect(runCli(bin, ["channel", "list", good]).stdout.trim()).toBe("");
});

test("apply — a batch that fails late leaves no version of anything it named", () => {
  // The invalid document sorts after several valid ones, so under the old
  // save-as-you-go loop every earlier definition was already committed.
  const names = [uid("aa"), uid("bb"), uid("cc")];
  const r = runCli(bin, [
    "apply", "-f",
    writeDefs([...names.map(switchDef), { name: uid("zz_bad"), tasks: [] }]),
  ]);
  expect(r.ok).toBe(false);

  const applied = defs(["--since", "1h"]).map((d) => d.name);
  for (const n of names) expect(applied).not.toContain(n);
});

test("apply — a rejected re-apply leaves the existing version untouched", () => {
  const name = uid("keep");
  runCli(bin, ["apply", "-f", writeDefs([switchDef(name)])]);

  // A valid v2 alongside a rejected sibling: neither lands, and v1 keeps the channel.
  const v2 = { ...switchDef(name), tasks: [{ id: "s2", switch: [{ goto: "end" }] }] };
  const r = runCli(bin, ["apply", "-f", writeDefs([v2, { name: uid("bad"), tasks: [] }])]);
  expect(r.ok).toBe(false);

  expect(defs(["--since", "1h"]).filter((d) => d.name === name).map((d) => d.version)).toEqual([1]);
  expect(runCli(bin, ["channel", "list", name]).stdout).toContain("latest -> v1");
});

// ── validate ────────────────────────────────────────────────────────────────────

test("validate — prints the resolved schema without registering anything", () => {
  const name = uid("proc");
  const r = runCli(bin, ["validate", "-f", writeDefs([switchDef(name)])]);

  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(name);
  // Validation is a dry run: nothing reaches the registry.
  expect(defs(["--since", "1h"]).some((d) => d.name === name)).toBe(false);
});

test("validate — exits non-zero for an invalid definition", () => {
  expect(runCli(bin, ["validate", "-f", writeDefs([{ name: uid("bad"), tasks: [] }])]).ok).toBe(false);
});

// ── definitions: displayed fields ───────────────────────────────────────────────

test("definitions — the table commits to NAME, VERSION, REGISTERED and RAISES", () => {
  const code = uid("code");
  const name = uid("raiser");
  runCli(bin, ["apply", "-f", writeDefs([raisingDef(name, code)])]);

  const lines = runCli(bin, ["definitions", "--since", "1h"]).stdout.trim().split("\n");
  expect(lines[0].split(/\s+/)).toEqual(["NAME", "VERSION", "REGISTERED", "RAISES"]);

  const row = lines.find((l) => l.startsWith(name))!;
  expect(row).toContain("v1");
  expect(row).toContain("just now"); // REGISTERED renders as a relative age
  // RAISES is derived by scanning raise clauses — there is no errors: block to read.
  expect(row).toContain(code);
});

test("definitions --json — the raw rows carry name, version and created_at", () => {
  const name = uid("proc");
  runCli(bin, ["apply", "-f", writeDefs([switchDef(name)])]);

  const row = defs(["--since", "1h"]).find((d) => d.name === name)!;
  expect(row.version).toBe(1);
  // RFC3339 in UTC: machine output never depends on the reader's zone.
  expect(row.created_at).toMatch(/^\d{4}-\d{2}-\d{2}T.*(Z|[+-]\d{2}:\d{2})$/);
});

// ── definitions: ordering and bounds ────────────────────────────────────────────

test("definitions --sort — name is alphabetical, created is registration order", () => {
  const stem = uid("sort");
  const [alpha, omega] = [`${stem}_aaa`, `${stem}_zzz`];
  // Registered zzz first, so the two orders disagree — which is what makes --sort
  // observable rather than incidentally the same list.
  runCli(bin, ["apply", "-f", writeDefs([switchDef(omega)])]);
  runCli(bin, ["apply", "-f", writeDefs([switchDef(alpha)])]);

  // Only the relative position of these two names is asserted, never the shape of the
  // whole list. Two reasons: the server is shared, so rows appear between calls; and the
  // rows come back in the *database's* collation, which the engines disagree on — SQLite
  // compares bytes ("big_values" < "bigctx", "_" being 0x5F) while Postgres under
  // en_US.utf8 demotes punctuation and yields the reverse. These names differ only after
  // a shared stem, so their order holds under either.
  const byName = defs(["--since", "1h", "--sort", "name"]).map((d) => d.name);
  expect(byName.indexOf(alpha)).toBeLessThan(byName.indexOf(omega));

  // The default sort is by registration, where the order is the other way round.
  const byCreated = defs(["--since", "1h"]).map((d) => d.name);
  expect(byCreated.indexOf(omega)).toBeLessThan(byCreated.indexOf(alpha));
});

test("definitions --since / --until — bound created_at, half-open", () => {
  const name = uid("bounds");
  runCli(bin, ["apply", "-f", writeDefs([switchDef(name)])]);

  expect(defs(["--since", "1h"]).some((d) => d.name === name)).toBe(true);
  // A window that closes before it was registered excludes it.
  expect(defs(["--since", "1h", "--until", "2000-01-01"])).toEqual([]);

  const bad = runCli(bin, ["definitions", "--since", "30"]);
  expect(bad.ok).toBe(false);
  expect(bad.stderr).toContain("invalid --since");
});

test("definitions — the cap keeps listCap rows and says so; --since lifts both", () => {
  const stem = uid("cap");
  runCli(bin, [
    "apply", "-f",
    writeDefs(
      Array.from({ length: listCap + 1 }, (_, i) =>
        switchDef(`${stem}_${String(i).padStart(2, "0")}`),
      ),
    ),
  ]);

  const capped = runCli(bin, ["definitions", "--json"]);
  expect((JSON.parse(capped.stdout) as DefRow[]).length).toBe(listCap);
  expect(capped.stderr).toContain(`showing the newest ${listCap} definitions`);
  expect(capped.stderr).toContain("--since");

  const full = runCli(bin, ["definitions", "--json", "--since", "1h"]);
  expect((JSON.parse(full.stdout) as DefRow[]).length).toBeGreaterThan(listCap);
  // Nothing was dropped, so claiming otherwise would send the reader chasing rows that
  // are already on screen.
  expect(full.stderr).toBe("");
});

test("definitions — under --sort name the cap keeps the first N, not the last", async () => {
  const stem = uid("dircap");
  runCli(bin, [
    "apply", "-f",
    writeDefs(
      Array.from({ length: listCap + 1 }, (_, i) =>
        switchDef(`${stem}_${String(i).padStart(2, "0")}`),
      ),
    ),
  ]);

  // Compared against the same list read uncapped, never against a JS sort: only the
  // database can say what name order is, and the engines' collations disagree.
  //
  // Both reads share an --until cutoff so they see the same rows. Without it this races:
  // the other suites apply definitions continuously, and one landing between the two
  // reads shifts the second list by a row. --since lifts the cap over that same window.
  const until = await frozenUntil();
  const all = defs(["--since", "2000-01-01", "--until", until, "--sort", "name"]).map((d) => d.name);
  const capped = defs(["--until", until, "--sort", "name"]).map((d) => d.name);

  expect(capped.length).toBe(listCap);
  expect(all.length).toBeGreaterThan(listCap);
  // A prefix, not a suffix — a newest-N cap on an A→Z list would return the right count
  // from the wrong end of the alphabet.
  expect(capped).toEqual(all.slice(0, listCap));
}, 15_000);

test("definitions — an empty result says so, and --json prints []", () => {
  // A window in the distant past can match nothing, whatever else the server holds.
  const r = runCli(bin, ["definitions", "--since", "1999-01-01", "--until", "2000-01-01"]);
  expect(r.ok).toBe(true);
  expect(r.stdout.trim()).toBe("no definitions");
  expect(r.stderr).toBe("");

  const j = runCli(bin, ["definitions", "--since", "1999-01-01", "--until", "2000-01-01", "--json"]);
  expect(j.stdout.trim()).toBe("[]");
});
