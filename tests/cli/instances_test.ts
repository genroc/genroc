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
  missingID,
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
  error_code: string;
  error_message: string;
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

test("get --json — a child task in a loop appears once, not once per iteration", async () => {
  const kid = uid("dupkid");
  const parent = uid("dupparent");
  const file = writeDefs([
    { name: kid, tasks: [{ id: "t", switch: [{ goto: "end" }] }], output: { ok: true } },
    {
      name: parent,
      tasks: [
        {
          id: "call",
          action: {
            type: "child",
            name: kid,
            input: {},
            result_schema: { type: "object", properties: { ok: { type: "boolean" } } },
          },
          output: { count: "$: (self.previous.count ?? 0) + 1" },
          switch: [{ case: "self.output.count >= 3", goto: "end" }, { goto: "$call" }],
        },
      ],
    },
  ]);
  expect(runCli(bin, ["apply", "-f", file]).ok).toBe(true);

  const id = startedID(runCli(bin, ["run", parent]).stdout);
  expect(await waitForInstance(id)).toBe("completed");

  // `outputs` holds ONE value per task however many times a loop re-enters it. Asserted on RAW
  // stdout because the failure this guards was a repeated KEY in the object, which JSON.parse
  // hides by silently keeping the last.
  const raw = runCli(bin, ["get", id, "--json"]).stdout;
  const outputs = raw.slice(raw.indexOf('"outputs"'));
  const occurrences = outputs.split(`"call":`).length - 1;
  expect(occurrences, `"call" appears ${occurrences}x in outputs:\n${raw}`).toBe(1);
});

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

  const { state } = JSON.parse(runCli(bin, ["get", id, "--json"]).stdout) as {
    state: { input: { count: number; name: string } };
  };
  expect(state.input.count).toBe(2); // --set won the field it named
  expect(state.input.name).toBe("Base"); // the untouched field survived
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

test("get — the detail block names the instance, its process and its state", () => {
  const name = apply(inputDef(uid("proc")));
  const id = startedID(runCli(bin, ["run", name, "--set", "count=42"]).stdout);

  const r = runCli(bin, ["get", id]);
  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(id);
  expect(r.stdout).toContain(`${name}@v1`);
  expect(r.stdout).toContain("Created:");
  expect(r.stdout).toContain("Updated:");
  expect(r.stdout).toContain("State:");
  expect(r.stdout).toContain("42"); // the input value lives in the context

  // --json is the raw server object, so it carries the context verbatim.
  const j = runCli(bin, ["get", id, "--json"]);
  expect(j.ok).toBe(true);
  expect(JSON.parse(j.stdout)).toMatchObject({ id, process: name, version: 1 });
});

// The error a failed instance REPORTS is an object on the wire — code, message, and whatever
// the clause attached. `get` decodes it as one; while it decoded a bare string, `get` on ANY
// failed instance died in the JSON decode instead of printing a thing.
test("get — a failed instance prints the error it reports, payload and all", async () => {
  const name = uid("panicky");
  runCli(bin, [
    "apply",
    "-f",
    writeDefs([
      {
        name,
        tasks: [
          {
            id: "go",
            switch: [
              {
                panic: {
                  code: "went_wrong",
                  message: "the upstream refused",
                  data: { retry_after: 3600 },
                },
              },
            ],
          },
        ],
      },
    ]),
  ]);
  const id = startedID(runCli(bin, ["run", name]).stdout);
  expect(await waitForInstance(id)).toBe("failed");

  const r = runCli(bin, ["get", id]);
  expect(r.ok, r.stderr).toBe(true);
  expect(r.stdout).toContain("the upstream refused");
  expect(r.stdout).toContain("went_wrong");
  expect(r.stdout, "the payload is the machine-readable half; printing only prose loses it").toContain(
    "retry_after",
  );
}, 15_000);

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
  expect(json.error_message.length).toBeGreaterThan(0);
  // The table bounds its last column at 50 chars + "..."; --json is the lossless form.
  const shown = row.slice(row.indexOf(json.error_code) + json.error_code.length).trim();
  expect(shown.length).toBeLessThanOrEqual(53);
  if (json.error_message.length > 50) expect(shown.endsWith("...")).toBe(true);
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

// ── roots-only default, and the filters ─────────────────────────────────────────

test("instances — lists roots only, and --children adds them back with a PARENT column", async () => {
  const kid = uid("rk");
  const parent = uid("rp");
  runCli(bin, [
    "apply",
    "-f",
    writeDefs([
      { name: kid, tasks: [{ id: "t", switch: [{ goto: "end" }] }], output: { ok: true } },
      {
        name: parent,
        tasks: [
          {
            id: "call",
            action: {
              type: "child",
              name: kid,
              input: {},
              result_schema: { type: "object", properties: { ok: { type: "boolean" } } },
            },
            switch: [{ goto: "end" }],
          },
        ],
      },
    ]),
  ]);
  const rootID = runCli(bin, ["run", parent, "-q"]).stdout.trim();
  expect(await waitForInstance(rootID)).toBe("completed");

  // A tree is one unit of work, so the default listing is one row for it.
  const roots = runCli(bin, ["instances", "--process", kid, "--since", "1h", "--json"]);
  expect(JSON.parse(roots.stdout), `${kid} only ever exists as a child`).toEqual([]);

  const withKids = JSON.parse(
    runCli(bin, ["instances", "--process", kid, "--children", "--since", "1h", "--json"])
      .stdout,
  ) as (InstanceRow & { parent_id: string })[];
  expect(withKids.length).toBe(1);
  expect(withKids[0].parent_id, "a child row must name its parent").toBe(rootID);

  // The PARENT column earns its width only when children are in the listing.
  const table = runCli(bin, ["instances", "--children", "--since", "1h"]).stdout;
  expect(table).toContain("PARENT");
  expect(runCli(bin, ["instances", "--since", "1h"]).stdout).not.toContain("PARENT");

  // The point of the default: -q yields only ids the root-only verbs can act on.
  const ids = runCli(bin, ["instances", "-q", "--since", "1h"]).stdout.trim().split("\n");
  expect(ids).toContain(rootID);
  expect(ids, "a child id here would be refused by pause/resume/retry").not.toContain(
    withKids[0].id,
  );
}, 30_000);

test("instances --process / --version — narrow to one process, and to one of its versions", () => {
  const name = apply(switchDef(uid("filt")));
  const mine = runCli(bin, ["run", name, "-q"]).stdout.trim();
  const other = apply(switchDef(uid("filt_other")));
  const theirs = runCli(bin, ["run", other, "-q"]).stdout.trim();

  const byProcess = runCli(bin, ["instances", "--process", name, "-q", "--since", "1h"])
    .stdout.trim()
    .split("\n");
  expect(byProcess).toContain(mine);
  expect(byProcess, "--process must exclude every other process").not.toContain(theirs);

  expect(
    runCli(bin, ["instances", "--process", name, "--version", "1", "-q", "--since", "1h"])
      .stdout.trim()
      .split("\n"),
  ).toContain(mine);
  // No instance is on version 99, so the pair narrows to nothing rather than ignoring one.
  expect(
    runCli(bin, ["instances", "--process", name, "--version", "99", "-q", "--since", "1h"])
      .stdout.trim(),
  ).toBe("");
}, 30_000);

// ── instances -q ────────────────────────────────────────────────────────────────

test("instances -q — ids only, one per line, and they match what the table lists", () => {
  const name = apply(switchDef(uid("quiet")));
  const ids = [
    runCli(bin, ["run", name, "-q"]).stdout.trim(),
    runCli(bin, ["run", name, "-q"]).stdout.trim(),
  ];

  // Scoped to this test's own process: the suite shares one server, so comparing two
  // whole-database listings races against every other file's instances.
  const scope = ["--process", name, "--since", "1h"];
  const q = runCli(bin, ["instances", "-q", ...scope]);
  expect(q.ok).toBe(true);
  const lines = q.stdout.trim().split("\n");
  expect(lines.every((l) => /^[0-9a-f-]{36}$/.test(l)), `not bare ids: ${q.stdout}`).toBe(true);
  expect(lines.sort()).toEqual([...ids].sort());

  // Same rows in the same order, so -q is a projection of the list and not its own query.
  const table = runCli(bin, ["instances", "--json", ...scope]);
  expect(runCli(bin, ["instances", "-q", ...scope]).stdout.trim().split("\n")).toEqual(
    (JSON.parse(table.stdout) as InstanceRow[]).map((r) => r.id),
  );
});

test("instances -q — an empty list prints NOTHING, not 'no instances'", () => {
  const r = runCli(bin, ["instances", "-q", "--error-code", uid("no_such_code")]);
  expect(r.ok).toBe(true);
  // The whole point: `genctl pause $(genctl instances -q ...)` would otherwise receive
  // the words "no" and "instances" as two instance ids.
  expect(r.stdout, "-q must put nothing on stdout when there is nothing to list").toBe("");
  expect(r.stderr).toBe("");
});

test("instances -q — feeding the ids straight into a lifecycle command", async () => {
  const name = apply(externalDef(uid("nested")));
  const ids = [
    startedID(runCli(bin, ["run", name]).stdout),
    startedID(runCli(bin, ["run", name]).stdout),
  ];
  for (const id of ids) await waitForExternalToken(id);

  // What `genctl pause $(genctl instances -q --status running)` does, with the shell's
  // word-splitting spelled out. Narrowed to this test's own ids before pausing: the suite
  // shares one server, so pausing every running instance suspends other tests mid-flight.
  const listed = runCli(bin, ["instances", "-q", "--status", "running", "--since", "1h"]);
  const args = listed.stdout.trim().split("\n").filter((id) => ids.includes(id));
  expect(args.sort(), "-q must list the running instances this test started").toEqual(
    [...ids].sort(),
  );

  const paused = runCli(bin, ["pause", ...args]);
  expect(paused.ok, paused.stderr).toBe(true);
  for (const id of ids) {
    expect((JSON.parse(runCli(bin, ["get", id, "--json"]).stdout) as InstanceRow).status).toBe(
      "paused",
    );
  }
}, 30_000);

test("instances -q — the cap still reports, and only on stderr", async () => {
  const name = apply(switchDef(uid("cap_quiet")));
  const ids = Array.from({ length: listCap + 1 }, () =>
    startedID(runCli(bin, ["run", name]).stdout),
  );
  expect(await waitForInstance(ids[ids.length - 1])).toBe("completed");

  const r = runCli(bin, ["instances", "-q"]);
  expect(r.stdout.trim().split("\n").length).toBe(listCap);
  // A truncated list nested into `pause` acts on 20 of 21 — so the notice matters more
  // here than anywhere, and must not land on stdout where it would become an argument.
  expect(r.stderr).toContain(`showing the newest ${listCap} instances`);
  expect(r.stdout).not.toContain("showing the newest");
}, 30_000);

test("instances — -q and --json are two machine formats, so naming both is refused", () => {
  const r = runCli(bin, ["instances", "-q", "--json"]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("two machine-readable forms");
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

// ── id lists ────────────────────────────────────────────────────────────────────
//
// pause/resume/retry act on every id named. They are assertions, so an id already in the
// asserted state is reported and forgiven — which is the property that lets a line that
// was only half applied be re-run as-is. specs/id-list-commands.md.

test("pause/resume — several ids at once, and re-running the same line converges", async () => {
  const name = apply(externalDef(uid("multipause")));
  const first = startedID(runCli(bin, ["run", name]).stdout);
  const second = startedID(runCli(bin, ["run", name]).stdout);
  const unnamed = startedID(runCli(bin, ["run", name]).stdout);
  for (const id of [first, second, unnamed]) await waitForExternalToken(id);
  const status = (id: string) =>
    (JSON.parse(runCli(bin, ["get", id, "--json"]).stdout) as InstanceRow).status;

  const r = runCli(bin, ["pause", first, second]);
  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(`paused: ${first}`);
  expect(r.stdout).toContain(`paused: ${second}`);
  expect(r.stderr).toContain("2 named: 2 paused, 0 already, 0 refused");
  expect(status(first)).toBe("paused");
  expect(status(second)).toBe("paused");
  expect(status(unnamed), "a list of ids must move only what it names").toBe("running");

  // The whole point of `already`: the same line, run again, is a no-op that SUCCEEDS.
  // Were an already-paused tree an error, a group that half applied could never be
  // repaired by repeating it.
  const again = runCli(bin, ["pause", first, second]);
  expect(again.ok, `re-running a satisfied assertion must exit 0: ${again.stderr}`).toBe(true);
  expect(again.stdout).toContain(`already: ${first}`);
  expect(again.stderr).toContain("0 paused, 2 already");
}, 30_000);

test("a refusal among the ids stops neither the rest nor the exit code", async () => {
  const name = apply(externalDef(uid("multirefuse")));
  const first = startedID(runCli(bin, ["run", name]).stdout);
  const second = startedID(runCli(bin, ["run", name]).stdout);
  for (const id of [first, second]) await waitForExternalToken(id);
  runCli(bin, ["pause", first, second]);

  // Named BETWEEN the two that can move: an abort would leave `second` paused, and a
  // swallowed refusal would exit 0.
  const r = runCli(bin, ["resume", first, missingID, second]);
  expect(r.ok, "a refusal among the ids must carry the exit code").toBe(false);
  expect(r.stderr).toContain(`genctl: ${missingID}:`);
  expect(r.stderr).toContain("3 named: 2 resumed, 0 already, 1 refused");

  const status = (id: string) =>
    (JSON.parse(runCli(bin, ["get", id, "--json"]).stdout) as InstanceRow).status;
  expect(status(first), "the id before the refusal must still have moved").toBe("running");
  expect(status(second), "the id after the refusal must still have moved").toBe("running");
}, 30_000);

test("resume — a settled tree is refused, not forgiven as 'already'", async () => {
  const name = apply(switchDef(uid("resume_settled")));
  const id = runCli(bin, ["run", name, "-q"]).stdout.trim();
  expect(await waitForInstance(id)).toBe("completed");

  // The split the server makes under its own lock: nothing is paused either way, but a
  // live tree satisfies "is advancing" and a settled one never will.
  const r = runCli(bin, ["resume", id]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("settled");

  // Same tree, opposite verb: it is not advancing, which is exactly what pause asserts.
  const p = runCli(bin, ["pause", id]);
  expect(p.ok, "pausing a settled tree asserts something already true").toBe(true);
  expect(p.stdout).toContain(`already: ${id}`);
}, 30_000);

test("retry — several ids re-arm in one command", async () => {
  const failing = apply(failingDef(uid("multiretry")));
  const first = runCli(bin, ["run", failing, "-q"]).stdout.trim();
  const second = runCli(bin, ["run", failing, "-q"]).stdout.trim();
  const ok = runCli(bin, ["run", apply(switchDef(uid("multiretry_ok"))), "-q"]).stdout.trim();
  expect(await waitForInstance(first)).toBe("failed");
  expect(await waitForInstance(second)).toBe("failed");
  expect(await waitForInstance(ok)).toBe("completed");

  // retry is an act, not an assertion: the completed one is refused rather than forgiven.
  const r = runCli(bin, ["retry", first, ok, second, "--force"]);
  expect(r.ok, "a refusal among the ids must carry the exit code").toBe(false);
  expect(r.stdout).toContain(`retried: ${first}`);
  expect(r.stdout, "the id after the refusal must still be retried").toContain(
    `retried: ${second}`,
  );
  expect(r.stderr).toContain("3 named: 2 retried, 0 already, 1 refused");
}, 30_000);

test("a malformed id list is refused whole — nothing is sent, nothing is mutated", async () => {
  const name = apply(externalDef(uid("badids")));
  const id = startedID(runCli(bin, ["run", name]).stdout);
  await waitForExternalToken(id);
  const status = () =>
    (JSON.parse(runCli(bin, ["get", id, "--json"]).stdout) as InstanceRow).status;

  // The mistake this exists for: `genctl pause $(genctl instances --status running)`
  // without -q, so the TABLE arrives as arguments. A real id sits among the words, and
  // acting on it while reporting "not found" for the rest is a typo that mutates.
  const table = runCli(bin, ["instances", "--status", "running", "--since", "1h"]);
  const words = table.stdout.split(/\s+/).filter(Boolean);
  expect(words, "the table must carry both headers and a real id").toContain("STATUS");
  expect(words).toContain(id);

  const r = runCli(bin, ["pause", ...words]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("are not instance ids");
  expect(r.stderr).toContain("nothing was sent");
  // One line, not one per cell: the noise was half the bug.
  expect(r.stderr.split("\n").filter((l) => l.startsWith("genctl:")).length).toBe(1);
  expect(r.stderr).toContain("-q");
  expect(status(), "a malformed command must not pause the id it happened to contain").toBe(
    "running",
  );

  // A single bad argument is a typo, so it gets no list hint to guess at.
  const one = runCli(bin, ["resume", "not-a-uuid"]);
  expect(one.ok).toBe(false);
  expect(one.stderr).toContain("is not an instance id");
  expect(one.stderr).not.toContain("-q");

  // The shape check must not cost @last, which is an id reference and not a UUID.
  expect(runCli(bin, ["pause", "@last"]).ok).toBe(true);
}, 30_000);

test("get and logs read one instance — a second id is refused, not dropped", () => {
  const name = apply(switchDef(uid("oneid")));
  const first = runCli(bin, ["run", name, "-q"]).stdout.trim();
  const second = runCli(bin, ["run", name, "-q"]).stdout.trim();

  const r = runCli(bin, ["get", first, second]);
  expect(r.ok, "an id that is silently dropped reads as if it had been shown").toBe(false);
  expect(r.stderr).toContain("get reads one instance");
  expect(runCli(bin, ["logs", first, second]).ok).toBe(false);
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
