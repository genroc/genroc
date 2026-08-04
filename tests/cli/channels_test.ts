import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { childDef, restDef, switchDef, uid } from "../helpers/genctl.ts";

// The channel entity: `channel list/set/delete` that move the pointers, `promote` that
// copies a whole channel forward, and `status` that reports whether a channel's baked
// child references still match what its channel points at.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

/** Apply definitions on a channel and return the channel name. */
function applyOn(defs: object[], channel: string): string {
  runCli(bin, ["apply", "-f", writeDefs(defs), "--channel", channel]);
  return channel;
}

// ── channel list / set / delete ─────────────────────────────────────────────────

test("channel — set points a channel, list shows it, delete removes it", () => {
  const name = uid("proc");
  runCli(bin, ["apply", "-f", writeDefs([restDef(name)])]);

  const set = runCli(bin, ["channel", "set", name, "stable", "1"]);
  expect(set.ok).toBe(true);
  expect(set.stdout).toContain(`set: ${name}@stable -> v1`);

  // list prints plain `channel -> vN` pointer lines: a projection, not a resource table.
  const list = runCli(bin, ["channel", "list", name]);
  expect(list.ok).toBe(true);
  expect(list.stdout).toContain("stable -> v1");

  const del = runCli(bin, ["channel", "delete", name, "stable"]);
  expect(del.ok).toBe(true);
  expect(del.stdout).toContain(`deleted: ${name}@stable`);
  expect(runCli(bin, ["channel", "list", name]).stdout).not.toContain("stable");
});

test("channel list — always shows latest, which apply maintains", () => {
  const name = uid("proc");
  runCli(bin, ["apply", "-f", writeDefs([switchDef(name)])]);
  expect(runCli(bin, ["channel", "list", name]).stdout).toContain("latest -> v1");

  const v2 = { ...switchDef(name), tasks: [{ id: "s2", switch: [{ goto: "end" }] }] };
  runCli(bin, ["apply", "-f", writeDefs([v2])]);
  // latest follows the newest version without being named.
  expect(runCli(bin, ["channel", "list", name]).stdout).toContain("latest -> v2");
});

test("channel — a process with no channels lists nothing rather than failing", () => {
  const r = runCli(bin, ["channel", "list", uid("never_applied")]);
  expect(r.ok).toBe(true);
  expect(r.stdout.trim()).toBe("");
});

test("channel set — rejects an unknown process and a non-numeric version", () => {
  expect(runCli(bin, ["channel", "set", uid("no_such"), "stable", "1"]).ok).toBe(false);

  const bad = runCli(bin, ["channel", "set", "p", "stable", "notanumber"]);
  expect(bad.ok).toBe(false);
  expect(bad.stderr).toContain("version must be a positive integer");
  // Zero and negatives are not versions either.
  expect(runCli(bin, ["channel", "set", "p", "stable", "0"]).ok).toBe(false);
});

// ── promote ─────────────────────────────────────────────────────────────────────

test("promote — copies every pointer from one channel to another", () => {
  const [a, b] = [uid("a"), uid("b")];
  const from = applyOn([switchDef(a), switchDef(b)], uid("from"));
  const to = uid("to");

  const r = runCli(bin, ["promote", "--from", from, "--to", to]);
  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(`promoted: ${a}@v1 -> ${to}`);
  expect(r.stdout).toContain(`promoted: ${b}@v1 -> ${to}`);

  for (const n of [a, b]) expect(runCli(bin, ["channel", "list", n]).stdout).toContain(`${to} -> v1`);
});

test("promote --process — moves only that process and its dependency subtree", () => {
  const child = uid("child");
  const parent = uid("parent");
  const unrelated = uid("unrelated");
  const from = applyOn([switchDef(child), childDef(parent, child), switchDef(unrelated)], uid("from"));
  const to = `${from}_out`;

  expect(runCli(bin, ["promote", "--from", from, "--to", to, "--process", parent]).ok).toBe(true);

  expect(runCli(bin, ["channel", "list", parent]).stdout).toContain(to);
  // The subtree comes along; anything outside it does not.
  expect(runCli(bin, ["channel", "list", child]).stdout).toContain(to);
  expect(runCli(bin, ["channel", "list", unrelated]).stdout).not.toContain(to);
});

test("promote — both --from and --to are required", () => {
  for (const args of [["promote", "--from", "staging"], ["promote", "--to", "prod"], ["promote"]]) {
    const r = runCli(bin, args);
    expect(r.ok).toBe(false);
    expect(r.stderr).toContain("--from and --to are required");
  }
});

// ── status ──────────────────────────────────────────────────────────────────────

test("status — reports a channel coherent while parent and child agree", () => {
  const child = uid("child");
  const parent = uid("parent");
  const channel = applyOn([switchDef(child), childDef(parent, child)], uid("track"));

  const r = runCli(bin, ["status", "--channel", channel]);
  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(`channel "${channel}" is coherent`);
});

test("status — reports a stale ref when a child advances without its parent", () => {
  const child = uid("child");
  const parent = uid("parent");
  const channel = applyOn([switchDef(child), childDef(parent, child)], uid("track"));

  // Advance the child alone: the parent still has v1 baked into its child reference.
  const child2 = { ...switchDef(child), tasks: [{ id: "s2", switch: [{ goto: "end" }] }] };
  applyOn([child2], channel);

  const r = runCli(bin, ["status", "--channel", channel]);
  expect(r.ok).toBe(true);
  expect(r.stdout).toContain("STALE");
  expect(r.stdout).toContain(parent);
  // The report names the task holding the reference and both versions in play.
  expect(r.stdout).toContain(`${child} baked@v1, channel@v2`);
  expect(r.stdout).toContain('task "spawn"');
});

test("status — stale refs are ordered deterministically by child name", () => {
  // Names chosen so the alphabetical order is independent of the random suffix and of
  // the child_map key order the server iterates (FindStaleRefs was once unordered).
  const childA = uid("aaa_child");
  const childB = uid("zzz_child");
  const parent = uid("parent");
  const channel = uid("track");
  runCli(bin, [
    "apply", "-f",
    writeDefs([
      switchDef(childA),
      switchDef(childB),
      {
        name: parent,
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
      },
    ]),
    "--channel", channel,
  ]);

  for (const c of [childA, childB]) {
    const next = { ...switchDef(c), tasks: [{ id: "s2", switch: [{ goto: "end" }] }] };
    applyOn([next], channel);
  }

  const out = runCli(bin, ["status", "--channel", channel]).stdout;
  expect(out.indexOf(childA)).toBeLessThan(out.indexOf(childB));
});

test("status — defaults to the latest channel", () => {
  const r = runCli(bin, ["status"]);
  expect(r.ok).toBe(true);
  expect(r.stdout).toMatch(/channel "latest" is coherent|STALE/);
});
