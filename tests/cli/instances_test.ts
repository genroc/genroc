import { mkdtempSync, writeFileSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { waitForInstance } from "../helpers/client.ts";
import {
  BIG_BLOB,
  blobInputDef,
  externalDef,
  failingDef,
  inputDef,
  listCap,
  raisingDef,
  startedID,
  switchDef,
  uid,
  waitForExternalToken,
} from "../helpers/genctl.ts";

// The instance entity: `run` that creates one, `get` and `instances` that read it,
// `pause`/`resume`/`retry` that move it, and `last`/@last that address it. Every flag of
// each, plus the fields the table and the detail block commit to.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

type InstanceRow = {
  id: string;
  process: string;
  version: number;
  status: string;
  error: string;
  error_code: string;
  created_at: string;
  updated_at: string;
};

function instances(extra: string[] = []): InstanceRow[] {
  return JSON.parse(runCli(bin, ["instances", "--json", ...extra]).stdout) as InstanceRow[];
}

/** Apply a definition and return its name, so each test owns its own process. */
function apply(def: object & { name: string }): string {
  runCli(bin, ["apply", "-f", writeDefs([def])]);
  return def.name;
}

// ── run ─────────────────────────────────────────────────────────────────────────

test("run — prints the id, process and version it started", () => {
  const name = apply(inputDef(uid("proc")));
  const r = runCli(bin, ["run", name, "--set", "count=3"]);

  expect(r.ok).toBe(true);
  expect(r.stdout).toContain("started:");
  expect(r.stdout).toContain(`${name}@v1`);
  expect(startedID(r.stdout)).toMatch(/^[0-9a-f-]{36}$/);
});

test("run — the three input sources: --set, --input and -f", () => {
  const name = apply(inputDef(uid("proc")));

  // --set: repeatable, dotted keys nest, values type-inferred.
  expect(runCli(bin, ["run", name, "--set", "count=3", "--set", "name=Sam"]).ok).toBe(true);
  // --input: a relaxed JSON literal (unquoted keys, bare values).
  expect(runCli(bin, ["run", name, "--input", "{count: 7, name: Sam}"]).ok).toBe(true);
  // -f: a bare, tab-completable path.
  const file = join(tmpdir(), `genroc_input_${Date.now()}.json`);
  writeFileSync(file, JSON.stringify({ count: 9, name: "Ada" }), "utf8");
  expect(runCli(bin, ["run", name, "-f", file]).ok).toBe(true);

  // --input and -f name the same slot, so together they are ambiguous rather than merged.
  const both = runCli(bin, ["run", name, "--input", "{count: 1}", "-f", file]);
  expect(both.ok).toBe(false);
  expect(both.stderr).toContain("not both");
});

test("run --set — overlays onto --input rather than replacing it", () => {
  const name = apply(inputDef(uid("proc")));
  const id = startedID(runCli(bin, ["run", name, "--input", "{count: 1, name: Base}", "--set", "count=2"]).stdout);

  const { context } = JSON.parse(runCli(bin, ["get", id, "--json"]).stdout) as {
    context: { input: { count: number; name: string } };
  };
  expect(context.input.count).toBe(2); // --set won the field it named
  expect(context.input.name).toBe("Base"); // the untouched field survived
});

test("run --version and --channel — pin which version starts", () => {
  const name = uid("pinned");
  const channel = uid("ch");
  runCli(bin, ["apply", "-f", writeDefs([switchDef(name)]), "--channel", channel]);
  const v2 = { ...switchDef(name), tasks: [{ id: "s2", switch: [{ goto: "end" }] }] };
  runCli(bin, ["apply", "-f", writeDefs([v2])]); // v2 on latest only

  expect(runCli(bin, ["run", name, "--version", "1"]).stdout).toContain(`${name}@v1`);
  // The channel still points at v1, so it resolves there rather than to the newest.
  expect(runCli(bin, ["run", name, "--channel", channel]).stdout).toContain(`${name}@v1`);
  // No pin at all takes the latest.
  expect(runCli(bin, ["run", name]).stdout).toContain(`${name}@v2`);
});

test("run -q — prints the bare id and nothing else", () => {
  const name = apply(inputDef(uid("proc")));
  const r = runCli(bin, ["run", name, "--set", "count=1", "-q"]);

  // Exactly the id, so id=$(genctl run … -q) needs no trimming or parsing.
  expect(r.stdout.trim()).toMatch(/^[0-9a-f-]{36}$/);
  expect(r.stdout).not.toContain("started:");
});

test("run — input that fails the schema is reported before anything starts", () => {
  const name = apply(inputDef(uid("proc")));

  const r = runCli(bin, ["run", name, "--set", "count=not-a-number"]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("input is not valid for");
  expect(r.stdout).toBe("");
  // Counted per-process rather than over the whole listing: the other CLI files are
  // starting instances on this same server while this test runs.
  expect(instances(["--since", "1h"]).filter((i) => i.process === name)).toEqual([]);
});

// ── get: displayed fields ───────────────────────────────────────────────────────

test("get — the detail block names the instance, its process and its context", () => {
  const name = apply(inputDef(uid("proc")));
  const id = startedID(runCli(bin, ["run", name, "--set", "count=42"]).stdout);

  const r = runCli(bin, ["get", id]);
  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(id);
  expect(r.stdout).toContain(`${name}@v1`);
  expect(r.stdout).toContain("Created:");
  expect(r.stdout).toContain("Updated:");
  expect(r.stdout).toContain("Context:");
  expect(r.stdout).toContain("42"); // the input value lives in the context

  // --json is the raw server object, so it carries the context verbatim.
  const j = runCli(bin, ["get", id, "--json"]);
  expect(j.ok).toBe(true);
  expect(JSON.parse(j.stdout)).toMatchObject({ id, process: name, version: 1 });
});

test("get --resolve — materializes context values held in the object store", async () => {
  const name = uid("bigctx");
  runCli(bin, ["apply", "-f", writeDefs([blobInputDef(name)])]);
  const id = runCli(bin, ["run", name, "--input", JSON.stringify({ blob: BIG_BLOB }), "-q"])
    .stdout.trim();
  expect(await waitForInstance(id)).toBe("completed");

  // Slot-level laziness: the context carries a reference, not the blob.
  const lazy = runCli(bin, ["get", id, "--json"]);
  expect(lazy.stdout).toContain(`"ref":`);
  expect(lazy.stdout).not.toContain("BBBBBBBBBB");

  const full = runCli(bin, ["get", id, "--resolve", "--json"]);
  expect(full.ok).toBe(true);
  expect(full.stdout).toContain("BBBBBBBBBB");
}, 15_000);

// ── instances: displayed fields ─────────────────────────────────────────────────

test("instances — the table commits to ID, STATUS, PROCESS, UPDATED, CREATED, CODE, ERROR", async () => {
  const code = uid("code");
  const name = apply(raisingDef(uid("raiser"), code));
  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("raised");

  const lines = runCli(bin, ["instances", "--since", "1h"]).stdout.trim().split("\n");
  expect(lines[0].split(/\s+/)).toEqual([
    "ID", "STATUS", "PROCESS", "UPDATED", "CREATED", "CODE", "ERROR",
  ]);

  const row = lines.find((l) => l.startsWith(id))!;
  expect(row).toContain("raised");
  expect(row).toContain(`${name}@v1`);
  expect(row).toContain("just now"); // UPDATED/CREATED render as relative ages
  expect(row).toContain(code); // CODE carries the authored error code
});

test("instances — a long error message is truncated in the table but whole in --json", async () => {
  const name = apply(failingDef(uid("longerr")));
  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("failed");

  const row = runCli(bin, ["instances", "--since", "1h"]).stdout
    .split("\n")
    .find((l) => l.startsWith(id))!;
  const json = instances(["--since", "1h"]).find((i) => i.id === id)!;
  expect(json.error.length).toBeGreaterThan(0);
  // The table bounds its last column at 50 chars + "..."; --json is the lossless form.
  const shown = row.slice(row.indexOf(json.error_code) + json.error_code.length).trim();
  expect(shown.length).toBeLessThanOrEqual(53);
  if (json.error.length > 50) expect(shown.endsWith("...")).toBe(true);
}, 15_000);

// ── instances: filters, ordering and bounds ─────────────────────────────────────

test("instances --status — narrows to one status", async () => {
  const name = apply(switchDef(uid("status_f")));
  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("completed");

  const completed = instances(["--since", "1h", "--status", "completed"]);
  expect(completed.some((i) => i.id === id)).toBe(true);
  expect(completed.every((i) => i.status === "completed")).toBe(true);
  expect(instances(["--since", "1h", "--status", "running"]).some((i) => i.id === id)).toBe(false);
});

test("instances --error-code — matches the authored code exactly", async () => {
  const code = uid("boom");
  const name = apply(raisingDef(uid("raiser"), code));
  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("raised");

  expect(instances(["--since", "1h", "--error-code", code]).map((i) => i.id)).toEqual([id]);
  // Exact, not a prefix: a near-miss selects nothing.
  expect(instances(["--since", "1h", "--error-code", code.slice(0, -1)])).toEqual([]);
});

test("instances --sort updated — orders by last activity, not creation", async () => {
  const name = apply(externalDef(uid("sort_upd")));
  const first = startedID(runCli(bin, ["run", name]).stdout);
  const second = startedID(runCli(bin, ["run", name]).stdout);
  await waitForExternalToken(second);

  // Resolving the first-created makes it the last-updated, which is what tells the two
  // sorts apart — under one order it is first, under the other last.
  runCli(bin, ["resolve", await waitForExternalToken(first), "--set", "approved=true"]);
  expect(await waitForInstance(first)).toBe("completed");

  const ids = (sort: string) => instances(["--since", "1h", "--sort", sort]).map((i) => i.id);
  const byCreated = ids("created");
  expect(byCreated.indexOf(first)).toBeLessThan(byCreated.indexOf(second));
  const byUpdated = ids("updated");
  expect(byUpdated.indexOf(second)).toBeLessThan(byUpdated.indexOf(first));

  runCli(bin, ["resolve", await waitForExternalToken(second), "--set", "approved=true"]);
  expect(await waitForInstance(second)).toBe("completed");
}, 30_000);

test("instances — display is oldest→newest, so the most recent row is last", async () => {
  const name = apply(switchDef(uid("order")));
  const ids = [0, 1, 2].map(() => startedID(runCli(bin, ["run", name]).stdout));
  for (const id of ids) expect(await waitForInstance(id)).toBe("completed");

  const listed = instances(["--since", "1h"]).map((i) => i.id);
  expect(listed.indexOf(ids[0])).toBeLessThan(listed.indexOf(ids[2]));

  // The capped path reaches its rows by reversing a descending fetch, so it must agree
  // with the streaming path on direction and not merely on contents.
  const times = instances().map((i) => new Date(i.created_at).getTime());
  expect(times).toEqual([...times].sort((a, b) => a - b));
}, 15_000);

test("instances --since / --until — bound whichever column --sort selects", async () => {
  const name = apply(switchDef(uid("since_inst")));
  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("completed");

  expect(instances(["--since", "1h"]).some((i) => i.id === id)).toBe(true);
  expect(instances(["--since", "1h", "--sort", "updated"]).some((i) => i.id === id)).toBe(true);
  expect(instances(["--since", "1h", "--until", "2000-01-01"])).toEqual([]);

  const bad = runCli(bin, ["instances", "--since", "30"]);
  expect(bad.ok).toBe(false);
  expect(bad.stderr).toContain("invalid --since");
});

test("instances — the cap keeps listCap rows and says so; --since lifts both", async () => {
  const name = apply(switchDef(uid("cap_inst")));
  const ids = Array.from({ length: listCap + 1 }, () =>
    startedID(runCli(bin, ["run", name]).stdout),
  );
  expect(await waitForInstance(ids[ids.length - 1])).toBe("completed");

  const capped = runCli(bin, ["instances", "--json"]);
  expect((JSON.parse(capped.stdout) as InstanceRow[]).length).toBe(listCap);
  expect(capped.stderr).toContain(`showing the newest ${listCap} instances`);

  const full = runCli(bin, ["instances", "--json", "--since", "1h"]);
  expect((JSON.parse(full.stdout) as InstanceRow[]).length).toBeGreaterThan(listCap);
  expect(full.stderr).toBe("");
}, 30_000);

test("instances — an empty result says so, and --json prints []", () => {
  const nothing = uid("no_such_code");
  const r = runCli(bin, ["instances", "--error-code", nothing]);
  expect(r.ok).toBe(true);
  expect(r.stdout.trim()).toBe("no instances");
  // An empty list was not truncated, so it must never carry the cap notice.
  expect(r.stderr).toBe("");
  expect(runCli(bin, ["instances", "--error-code", nothing, "--json"]).stdout.trim()).toBe("[]");
});

// ── pause / resume / retry ──────────────────────────────────────────────────────

test("pause then resume — parks a running instance and revives it", async () => {
  const name = apply(externalDef(uid("pausable")));
  const id = startedID(runCli(bin, ["run", name]).stdout);
  const token = await waitForExternalToken(id);

  expect(runCli(bin, ["pause", id]).ok).toBe(true);
  expect(instances(["--since", "1h"]).find((i) => i.id === id)?.status).toBe("paused");

  expect(runCli(bin, ["resume", id]).ok).toBe(true);
  expect(instances(["--since", "1h"]).find((i) => i.id === id)?.status).toBe("running");

  runCli(bin, ["resolve", token, "--set", "approved=true"]);
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);

test("retry — re-arms a failed instance; --force also overrides only_once", async () => {
  const name = apply(failingDef(uid("retryable")));
  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("failed");

  expect(runCli(bin, ["retry", id]).ok).toBe(true);
  expect(await waitForInstance(id)).toBe("failed"); // it fails again, having been re-run
  expect(runCli(bin, ["retry", id, "--force"]).ok).toBe(true);
  expect(await waitForInstance(id)).toBe("failed");
}, 30_000);

test("retry — refuses an instance that has not failed", async () => {
  const name = apply(switchDef(uid("retry_ok")));
  const id = runCli(bin, ["run", name, "-q"]).stdout.trim();
  expect(await waitForInstance(id)).toBe("completed");

  const r = runCli(bin, ["retry", id]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("genctl: ");
});

// ── last / @last ────────────────────────────────────────────────────────────────

test("last and @last — address the most recently started instance", () => {
  const name = apply(inputDef(uid("proc")));
  const id = runCli(bin, ["run", name, "--set", "count=7", "-q"]).stdout.trim();

  expect(runCli(bin, ["last"]).stdout.trim()).toBe(id);
  expect(runCli(bin, ["get", "@last", "--json"]).stdout).toContain(`"${id}"`);
  expect(runCli(bin, ["logs", "@last"]).ok).toBe(true);
});

test("@last — never implied, and errors when nothing has been run", () => {
  const name = apply(inputDef(uid("proc")));
  runCli(bin, ["run", name, "--set", "count=1", "-q"]);

  // Even straight after a run, a bare `get` must not silently reuse the last id.
  const bare = runCli(bin, ["get"]);
  expect(bare.ok).toBe(false);
  expect(bare.stderr).toContain("instance id is required");

  // A pristine config home has no recorded id to resolve.
  const home = mkdtempSync(join(tmpdir(), "genroc_nolast_"));
  const r = runCli(bin, ["get", "@last"], { HOME: home, XDG_CONFIG_HOME: join(home, ".config") });
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("no instance recorded");
});
