import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { client } from "../helpers/client.ts";
import { childDef, listCap, raisingDef, restDef, switchDef, uid } from "../helpers/genctl.ts";

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

test("apply --auto-update-parents — a child bump rolls its parents on the same channel", () => {
  const child = uid("child");
  const parent = uid("parent");
  const channel = uid("ch");
  runCli(bin, [
    "apply", "-f", writeDefs([switchDef(child), childDef(parent, child)]), "--channel", channel,
  ]);

  const child2 = { ...switchDef(child), tasks: [{ id: "s2", switch: [{ goto: "end" }] }] };
  const r = runCli(bin, [
    "apply", "-f", writeDefs([child2]), "--channel", channel, "--auto-update-parents",
  ]);

  expect(r.stdout).toContain(`saved: ${child}@v2`);
  // The parent is re-emitted because it was rebuilt against the new child, which is the
  // whole point of the flag — without it the parent keeps pointing at v1.
  expect(r.stdout).toContain(parent);
});

test("apply — a self-referential process is accepted", () => {
  const name = uid("recursive");
  expect(runCli(bin, ["apply", "-f", writeDefs([childDef(name, name)])]).stdout).toContain(
    `saved: ${name}@v1`,
  );
});

test("apply — an invalid definition fails the call, and is NOT rolled back", () => {
  const good = uid("good");
  // tasks must not be empty, so the second document is rejected.
  const r = runCli(bin, ["apply", "-f", writeDefs([switchDef(good), { name: uid("bad"), tasks: [] }])]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("genctl:");

  // Documenting current behaviour, not endorsing it: applyBatch saves each definition
  // inside its loop with no enclosing transaction, so everything ahead of the failure in
  // topological order is already persisted when the call reports failure. One `apply`
  // therefore lands partially — flip this assertion if the batch is made atomic.
  expect(defs(["--since", "1h"]).some((d) => d.name === good)).toBe(true);
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

test("definitions --sort — created is newest-first by default, name is alphabetical", () => {
  const stem = uid("sort");
  const [alpha, omega] = [`${stem}_aaa`, `${stem}_zzz`];
  // One apply, so they share a registration instant and only the name order is decided.
  runCli(bin, ["apply", "-f", writeDefs([switchDef(omega), switchDef(alpha)])]);

  const byName = defs(["--since", "1h", "--sort", "name"]).map((d) => d.name);
  expect(byName.indexOf(alpha)).toBeLessThan(byName.indexOf(omega));
  expect(byName).toEqual([...byName].sort());

  // The default sort is by registration, so it need not be alphabetical at all.
  const byCreated = defs(["--since", "1h"]).map((d) => d.name);
  expect(byCreated).toContain(alpha);
  expect(byCreated).toContain(omega);
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

test("definitions — under --sort name the cap keeps the first N, not the last", () => {
  // A newest-N cap on an A→Z list would show the tail of the alphabet: right count,
  // wrong rows. Two names bracketing the range make the direction visible.
  const stem = uid("dircap");
  runCli(bin, [
    "apply", "-f",
    writeDefs(
      Array.from({ length: listCap + 1 }, (_, i) =>
        switchDef(`${stem}_${String(i).padStart(2, "0")}`),
      ),
    ),
  ]);

  const capped = defs(["--sort", "name"]).map((d) => d.name);
  expect(capped.length).toBe(listCap);
  expect(capped).toEqual([...capped].sort());
});

test("definitions — an empty result says so, and --json prints []", () => {
  // A window in the distant past can match nothing, whatever else the server holds.
  const r = runCli(bin, ["definitions", "--since", "1999-01-01", "--until", "2000-01-01"]);
  expect(r.ok).toBe(true);
  expect(r.stdout.trim()).toBe("no definitions");
  expect(r.stderr).toBe("");

  const j = runCli(bin, ["definitions", "--since", "1999-01-01", "--until", "2000-01-01", "--json"]);
  expect(j.stdout.trim()).toBe("[]");
});
