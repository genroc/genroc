import { writeFileSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
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

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

type DefRow = { name: string; version: number; created_at: string; raises?: string[] };

function defs(extra: string[] = []): DefRow[] {
  return JSON.parse(runCli(bin, ["definitions", "--json", ...extra]).stdout) as DefRow[];
}

// ── apply ───────────────────────────────────────────────────────────────────────

test("apply — a YAML merge key folds the anchored map in, and an explicit key overrides it", () => {
  const name = uid("merge");
  const file = join(tmpdir(), `${name}.yaml`);
  // `two` declares no action type or method of its own, so it validates only if the merge landed.
  writeFileSync(
    file,
    [
      `name: ${name}`,
      "tasks:",
      "  - id: one",
      "    action: &defaults",
      "      type: fetch",
      "      method: get",
      '      url: "https://example.test/a"',
      "      timeout: 7s",
      "    switch: [{ goto: $two }]",
      "  - id: two",
      "    action:",
      "      <<: *defaults",
      '      url: "https://example.test/b"',
      "      timeout: 9s",
      "    switch: [{ goto: end }]",
      "",
    ].join("\n"),
    "utf8",
  );

  expect(runCli(bin, ["apply", "-f", file]).stdout).toContain(`latest: ${name} - -> v1 (new)`);
});


test("check — a child that exists only in the batch resolves, as it does on apply", () => {
  const child = uid("vchild");
  const parent = uid("vparent");
  const file = writeDefs([switchDef(child), childDef(parent, child)]);

  const r = runCli(bin, ["apply", "--check-only", "-f", file]);
  expect(r.stderr).not.toContain("no definitions found");
  expect(r.ok).toBe(true);
});


test("apply — reports the move the channel made, not just whether a version was written", () => {
  const name = uid("proc");
  const a = writeDefs([switchDef(name)]);
  const b = writeDefs([{ ...switchDef(name), tasks: [{ id: "s2", switch: [{ goto: "end" }] }] }]);

  expect(
    runCli(bin, ["apply", "-f", a]).stdout.trim(),
    "`-` stands in for a channel that had no pointer yet",
  ).toBe(`latest: ${name} - -> v1 (new)`);
  expect(runCli(bin, ["apply", "-f", b]).stdout.trim()).toBe(`latest: ${name} v1 -> v2 (new)`);

  expect(
    runCli(bin, ["apply", "-f", b]).stdout.trim(),
    "re-applying what the channel already points at moved nothing",
  ).toBe(`latest: ${name} v2 (current)`);
  expect(
    runCli(bin, ["apply", "-f", a]).stdout.trim(),
    "a revert walks the pointer BACKWARDS onto a stored version -- the whole point of the line",
  ).toBe(`latest: ${name} v2 -> v1 (existing)`);
});

test("apply --channel — `previous` is that channel's pointer, not the default's", () => {
  const name = uid("proc");
  const a = writeDefs([switchDef(name)]);
  const b = writeDefs([{ ...switchDef(name), tasks: [{ id: "s2", switch: [{ goto: "end" }] }] }]);

  runCli(bin, ["apply", "-f", a]); // latest -> v1
  runCli(bin, ["apply", "-f", b]); // latest -> v2

  // prod has no pointer yet, so there is no move to report even though latest is at v2.
  expect(runCli(bin, ["apply", "-f", a, "--channel", "prod"]).stdout.trim()).toBe(
    `prod: ${name} - -> v1 (existing)`,
  );
  expect(
    runCli(bin, ["apply", "-f", b, "--channel", "prod"]).stdout.trim(),
    "prod moves v1 -> v2 while latest sat at v2 the whole time",
  ).toBe(`prod: ${name} v1 -> v2 (existing)`);
});

test("apply — changed content mints the next version", () => {
  const name = uid("proc");
  runCli(bin, ["apply", "-f", writeDefs([switchDef(name)])]);
  const changed = { ...switchDef(name), tasks: [{ id: "s2", switch: [{ goto: "end" }] }] };

  expect(runCli(bin, ["apply", "-f", writeDefs([changed])]).stdout).toContain(
    `latest: ${name} v1 -> v2 (new)`,
  );
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
  for (const n of [a, b, c]) expect(r.stdout).toContain(`latest: ${n} - -> v1 (new)`);
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
    `latest: ${name} - -> v1 (new)`,
  );
});

test("apply — one invalid definition rolls the whole batch back", () => {
  const good = uid("good");
  // tasks must not be empty, so the second document is rejected.
  const r = runCli(bin, ["apply", "-f", writeDefs([switchDef(good), { name: uid("bad"), tasks: [] }])]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("genctl:");

  expect(defs(["--since", "1h"]).some((d) => d.name === good)).toBe(false);
  expect(runCli(bin, ["channel", "list", good]).stdout.trim()).toBe("");
});

test("apply — a batch that fails late leaves no version of anything it named", () => {
  // The invalid document comes last, so a save-as-you-go apply would have committed the rest.
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

// ── apply --check-only ──────────────────────────────────────────────────────────

test("apply --check-only — reports each definition without registering anything", () => {
  const name = uid("proc");
  const file = writeDefs([switchDef(name)]);
  const r = runCli(bin, ["apply", "--check-only", "-f", file]);

  expect(r.ok, r.stderr).toBe(true);
  expect(r.stdout.trim(), "the same per-definition line an apply prints, minus the version").toBe(
    `valid: ${name}`,
  );
  expect(defs(["--since", "1h"]).some((d) => d.name === name)).toBe(false);

  // --json is the inferred schemas, which is what a type generator reads.
  const j = runCli(bin, ["apply", "--check-only", "--json", "-f", file]);
  expect(j.ok, j.stderr).toBe(true);
  expect(JSON.parse(j.stdout)[0].process).toBe(name);
  expect(defs(["--since", "1h"]).some((d) => d.name === name)).toBe(false);
});

test("apply --check-only — exits non-zero for an invalid definition", () => {
  expect(
    runCli(bin, ["apply", "--check-only", "-f", writeDefs([{ name: uid("bad"), tasks: [] }])]).ok,
  ).toBe(false);
});

// ── definitions: displayed fields ───────────────────────────────────────────────

test("definitions — the table commits to NAME, VERSION, REGISTERED, BY and RAISES", () => {
  const code = uid("code");
  const name = uid("raiser");
  runCli(bin, ["apply", "-f", writeDefs([raisingDef(name, code)])]);

  const lines = runCli(bin, ["definitions", "--since", "1h"]).stdout.trim().split("\n");
  expect(lines[0].split(/\s+/)).toEqual(["NAME", "VERSION", "REGISTERED", "BY", "RAISES"]);

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
  // Registered zzz first, so the two orders disagree.
  runCli(bin, ["apply", "-f", writeDefs([switchDef(omega)])]);
  runCli(bin, ["apply", "-f", writeDefs([switchDef(alpha)])]);

  // Relative order only: the server is shared, and SQLite and Postgres collate punctuation
  // differently. These names differ only after a shared stem, so their order holds under either.
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

  // Against the uncapped list, not a JS sort: the engines' collations disagree. The shared
  // --until keeps a definition another suite applies between the two reads out of both.
  const until = await frozenUntil();
  const all = defs(["--since", "2000-01-01", "--until", until, "--sort", "name"]).map((d) => d.name);
  const capped = defs(["--until", until, "--sort", "name"]).map((d) => d.name);

  expect(capped.length).toBe(listCap);
  expect(all.length).toBeGreaterThan(listCap);
  // A newest-N cap on an A→Z list would return the right count from the wrong end.
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
