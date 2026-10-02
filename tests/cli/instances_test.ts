import { mkdtempSync, writeFileSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";

// Digit-led, so a process name cannot match it.
const ID_RE = /^[0-9][0-9a-hjkmnp-tv-z]{7,13}$/;
import { client, waitForInstance } from "../helpers/client.ts";
import {
  BIG_BLOB,
  blobInputDef,
  childDef,
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

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

type InstanceRow = {
  id: string;
  process: string;
  version: number;
  status: string;
  phase?: string;
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

test("detail --json — a child task in a loop appears once, not once per iteration", async () => {
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

  // Raw stdout: JSON.parse would hide a repeated `outputs` key by keeping the last.
  const raw = runCli(bin, ["detail", id, "--json"]).stdout;
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
  expect(startedID(r.stdout)).toMatch(ID_RE);
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

  const { state } = JSON.parse(runCli(bin, ["detail", id, "--json"]).stdout) as {
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
  expect(r.stdout.trim()).toMatch(ID_RE);
  expect(r.stdout).not.toContain("started:");
});

test("run — input that fails the schema is reported before anything starts", () => {
  const name = apply(inputDef(uid("proc")));

  const r = runCli(bin, ["run", name, "--set", "count=not-a-number"]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("input is not valid for");
  expect(r.stdout).toBe("");
  // Per-process: other CLI files start instances on this server concurrently.
  expect(instances(["--since", "1h"]).filter((i) => i.process === name)).toEqual([]);
});

// ── get: displayed fields ───────────────────────────────────────────────────────

// Keep the views apart: state is engine bookkeeping, and an everyday read must not hand it back.
test("get — reports the output and no state; detail reports the state", async () => {
  const name = uid("split");
  runCli(bin, [
    "apply",
    "-f",
    writeDefs([{
      name,
      input_schema: {
        type: "object",
        properties: { who: { type: "string" } },
        required: ["who"],
      },
      tasks: [{ id: "greet", output: { greeting: "hi, ${input.who}" }, switch: [{ goto: "end" }] }],
      output: { greeting: "$: outputs.greet.greeting" },
    }]),
  ]);
  const id = runCli(bin, ["run", name, "--set", "who=ada", "-q"]).stdout.trim();
  expect(await waitForInstance(id)).toBe("completed");

  const got = runCli(bin, ["get", id]);
  expect(got.ok, got.stderr).toBe(true);
  expect(got.stdout, "the declared output: block is what a caller came for").toContain(
    "greeting: hi, ada",
  );
  expect(
    got.stdout,
    "state is the engine's own slots — `get` handing them back is what `detail` exists to avoid",
  ).not.toContain("State:");
  expect(got.stdout).not.toContain("outputs:");

  const detail = runCli(bin, ["detail", id]);
  expect(detail.ok, detail.stderr).toBe(true);
  expect(detail.stdout).toContain("State:");
  expect(detail.stdout, "the per-task slot only detail carries").toContain("outputs:");
  expect(
    detail.stdout,
    "the server moves output out of state, so detail must print it or it is in neither",
  ).toContain("Output:");

  // --json is the machine form and stays JSON in both.
  const j = JSON.parse(runCli(bin, ["get", id, "--json"]).stdout) as {
    output: { greeting: string };
    state?: unknown;
  };
  expect(j.output.greeting).toBe("hi, ada");
  expect(j.state, "the status endpoint carries no state at all").toBeUndefined();
}, 15_000);

test("detail — a parent prints its children, a child its parent", async () => {
  const child = uid("kid");
  const parent = uid("parent");
  runCli(bin, ["apply", "-f", writeDefs([switchDef(child), childDef(parent, child)])]);
  const id = runCli(bin, ["run", parent, "-q"]).stdout.trim();
  expect(await waitForInstance(id)).toBe("completed");

  const kidID = (JSON.parse(runCli(bin, ["detail", id, "--json"]).stdout) as {
    children: { spawn: { out: string } };
  }).children.spawn.out;

  const r = runCli(bin, ["detail", id]);
  expect(r.ok, r.stderr).toBe(true);
  expect(r.stdout, "children are on the wire; the text view dropped them").toMatch(
    new RegExp(`Children:\\s+spawn:\\s+out: ${kidID}`),
  );
  expect(r.stdout).toContain("Epochs:");

  const k = runCli(bin, ["detail", kidID]);
  expect(k.ok, k.stderr).toBe(true);
  expect(k.stdout).toMatch(new RegExp(`Parent:\\s+${id}  \\(spawned by spawn\\)`));
}, 15_000);

test("detail — an external task reads as unclaimed, claimed, or its claim expired", async () => {
  const name = apply(externalDef(uid("ext")));
  const id = startedID(runCli(bin, ["run", name]).stdout);
  await waitForExternalToken(id);
  const external = () => {
    const r = runCli(bin, ["detail", id]);
    expect(r.ok, r.stderr).toBe(true);
    return r.stdout.match(/^External:\s+(.*)$/m)?.[1];
  };
  const claim = async (worker: string, lease_ms: number) => {
    const { data } = await client.POST("/external-tasks/claim", {
      body: { worker_id: worker, process: name, lease_ms } as never,
    });
    expect((data as { items: unknown[] }).items, "the parked task must be claimable").toHaveLength(1);
  };

  expect(external()).toBe("unclaimed");

  await claim("w1", 1);
  await new Promise((r) => setTimeout(r, 20));
  expect(
    external(),
    "expiry leaves the holder on the row, so printing it bare would claim w1 still has it",
  ).toBe("expired just now, was claimed by w1");

  await claim("w2", 60_000);
  expect(external()).toMatch(/^claimed by w2, \d+s left$/);
}, 15_000);

test("detail — the block names the instance, its process and its state", () => {
  const name = apply(inputDef(uid("proc")));
  const id = startedID(runCli(bin, ["run", name, "--set", "count=42"]).stdout);

  const r = runCli(bin, ["detail", id]);
  expect(r.ok).toBe(true);
  expect(r.stdout).toContain(id);
  expect(r.stdout).toContain(`${name}@v1`);
  expect(r.stdout).toContain("Created:");
  expect(r.stdout).toContain("Updated:");
  expect(r.stdout).toContain("State:");
  expect(r.stdout).toContain("42"); // the input value lives in the context

  // --json is the raw server object, so it carries the context verbatim.
  const j = runCli(bin, ["detail", id, "--json"]);
  expect(j.ok).toBe(true);
  expect(JSON.parse(j.stdout)).toMatchObject({ id, process: name, version: 1 });
});

// The reported error is an object on the wire (code, message, data), not a string.
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

test("detail — an externalized value shows its ref where the value belongs", async () => {
  const name = uid("refplace");
  runCli(bin, ["apply", "-f", writeDefs([blobInputDef(name)])]);
  const id = runCli(bin, ["run", name, "--input", JSON.stringify({ blob: BIG_BLOB }), "-q"])
    .stdout.trim();
  expect(await waitForInstance(id)).toBe("completed");

  const text = runCli(bin, ["detail", id]);
  expect(text.ok, text.stderr).toBe(true);
  const state = text.stdout.slice(text.stdout.indexOf("State:"));
  expect(
    state,
    "the slot is absent on the wire, so a reader who cannot see the key cannot tell a value "
      + "that was cut from one that was never there",
  ).toMatch(/blob:\s+ref: [0-9a-f]{32}\s+size: \d+/);
  expect(state).not.toContain("BBBBBBBBBB");

  // --resolve means the same thing here as it does in --json.
  const resolved = runCli(bin, ["detail", id, "--resolve"]);
  expect(resolved.ok, resolved.stderr).toBe(true);
  expect(resolved.stdout).toContain("BBBBBBBBBB");
  expect(resolved.stdout).not.toContain('"ref"');
}, 15_000);

test("detail --resolve — materializes context values held in the object store", async () => {
  const name = uid("bigctx");
  runCli(bin, ["apply", "-f", writeDefs([blobInputDef(name)])]);
  const id = runCli(bin, ["run", name, "--input", JSON.stringify({ blob: BIG_BLOB }), "-q"])
    .stdout.trim();
  expect(await waitForInstance(id)).toBe("completed");

  // Slot-level laziness: the context carries a reference, not the blob.
  const lazy = runCli(bin, ["detail", id, "--json"]);
  expect(lazy.stdout).toContain(`"ref":`);
  expect(lazy.stdout).not.toContain("BBBBBBBBBB");

  const full = runCli(bin, ["detail", id, "--resolve", "--json"]);
  expect(full.ok).toBe(true);
  expect(full.stdout).toContain("BBBBBBBBBB");
}, 15_000);

// An array index arrives as a json.Number, not the float64 a hand-built path would hold.
test("get — an externalized array element is marked and spliced at its index", async () => {
  const name = uid("refindex");
  const [first, second] = ["A".repeat(20 * 1024), "C".repeat(20 * 1024)];
  runCli(bin, ["apply", "-f", writeDefs([{
    name,
    input_schema: {
      type: "object",
      properties: { blobs: { type: "array", items: { type: "string" } } },
      required: ["blobs"],
    },
    output: { items: "$: input.blobs" },
    tasks: [{ id: "s1", switch: [{ goto: "end" }] }],
  }])]);
  const id = runCli(bin, ["run", name, "--input", JSON.stringify({ blobs: [first, second] }), "-q"])
    .stdout.trim();
  expect(await waitForInstance(id)).toBe("completed");

  const raw = JSON.parse(runCli(bin, ["get", id, "--json"]).stdout);
  expect(
    raw.objects.map((o: { path: unknown[] }) => o.path),
    "the premise: each element is cut at its own index",
  ).toEqual(expect.arrayContaining([["output", "items", 0], ["output", "items", 1]]));

  const text = runCli(bin, ["get", id]);
  expect(text.ok, text.stderr).toBe(true);
  expect(text.stdout, "each cut element shows its ref where it belongs, not null").toMatch(
    /items:\s+- ref: [0-9a-f]{32}\s+size: \d+\s+- ref: [0-9a-f]{32}\s+size: \d+/,
  );

  const resolved = runCli(bin, ["get", id, "--resolve", "--json"]);
  expect(resolved.ok, resolved.stderr).toBe(true);
  expect(JSON.parse(resolved.stdout).output.items, "--resolve puts each element back in place")
    .toEqual([first, second]);
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

test("instances --phase — lists what is parked, which status cannot say", async () => {
  const name = apply(externalDef(uid("wait_f")));
  const parked = startedID(runCli(bin, ["run", name]).stdout);
  await waitForExternalToken(parked);

  const done = startedID(runCli(bin, ["run", apply(switchDef(uid("wait_done")))]).stdout);
  expect(await waitForInstance(done)).toBe("completed");
  const napDef = {
    name: uid("wait_nap"),
    tasks: [{ id: "nap", action: { type: "delay", for: "1h" }, switch: "end" }],
  };
  const napping = startedID(runCli(bin, ["run", apply(napDef)]).stdout);

  // One listing: comparing against a second is racy, as other files settle rows in between.
  const external = instances(["--since", "1h", "--phase", "external"]);
  expect(external.find((i) => i.id === parked)?.status, "parked is still running").toBe("running");
  expect(external.every((i) => i.phase === "external")).toBe(true);
  expect(external.some((i) => i.id === done)).toBe(false);
  expect(
    external.some((i) => i.id === napping),
    "a running row that is not parked; a filter that fell back to status would list it",
  ).toBe(false);
  expect(
    instances(["--since", "1h", "--status", "running"]).some((i) => i.id === napping),
    "the premise: the napping row is running",
  ).toBe(true);

  // Answering it empties the filter of that row -- the listing tracks the park, not the run.
  runCli(bin, ["signal", parked, "--task", "approval", "--set", "approved=true"]);
  expect(await waitForInstance(parked)).toBe("completed");
  expect(instances(["--since", "1h", "--phase", "external"]).some((i) => i.id === parked)).toBe(false);
}, 15_000);

test("instances — the STATUS column carries phase, and only where there is one", async () => {
  const name = apply(externalDef(uid("wait_col")));
  const parked = startedID(runCli(bin, ["run", name]).stdout);
  await waitForExternalToken(parked);
  const done = startedID(runCli(bin, ["run", apply(switchDef(uid("wait_col_done")))]).stdout);
  expect(await waitForInstance(done)).toBe("completed");

  const lines = runCli(bin, ["instances", "--since", "1h"]).stdout.trim().split("\n");
  // No column of its own: the header is the same one the table has always committed to.
  expect(lines[0].split(/\s+/)).toEqual([
    "ID", "STATUS", "PROCESS", "UPDATED", "CREATED", "CODE", "ERROR",
  ]);
  expect(lines.find((l) => l.startsWith(parked))!).toContain("running\u00b7external");
  // A row with no wait state prints the bare status, not a trailing separator.
  expect(lines.find((l) => l.startsWith(done))!).not.toContain("\u00b7");

  runCli(bin, ["signal", parked, "--task", "approval", "--set", "approved=true"]);
  expect(await waitForInstance(parked)).toBe("completed");
}, 15_000);

test("instances --task — the position, which only --process narrows to one task", async () => {
  // Two definitions share the task id, so only --process narrows it to one.
  const shared = "review";
  const def = (prefix: string, task: string) => ({
    name: uid(prefix),
    tasks: [{ id: task, switch: [{ goto: "end" }] }],
  });
  const one = apply(def("task_a", shared));
  const two = apply(def("task_b", shared));
  const other = apply(def("task_c", "elsewhere"));

  const a = startedID(runCli(bin, ["run", one]).stdout);
  const b = startedID(runCli(bin, ["run", two]).stdout);
  const c = startedID(runCli(bin, ["run", other]).stdout);
  for (const id of [a, b, c]) expect(await waitForInstance(id)).toBe("completed");

  // A terminal instance still carries the task it stopped on, so the filter reaches it.
  const onShared = instances(["--since", "1h", "--task", shared]).map((i) => i.id);
  expect(onShared).toContain(a);
  expect(onShared).toContain(b);
  expect(onShared).not.toContain(c);

  const scoped = instances(["--since", "1h", "--task", shared, "--process", one]).map((i) => i.id);
  expect(scoped).toEqual([a]);

  // Exact, not a prefix.
  expect(instances(["--since", "1h", "--task", shared.slice(0, -1)])).toEqual([]);
}, 15_000);

test("instances --sort updated — orders by last activity, not creation", async () => {
  const name = apply(externalDef(uid("sort_upd")));
  const first = startedID(runCli(bin, ["run", name]).stdout);
  const second = startedID(runCli(bin, ["run", name]).stdout);
  await waitForExternalToken(first);
  await waitForExternalToken(second);

  // Resolving the first-created makes it the last-updated, so the two sorts disagree.
  runCli(bin, ["signal", first, "--task", "approval", "--set", "approved=true"]);
  expect(await waitForInstance(first)).toBe("completed");

  const ids = (sort: string) => instances(["--since", "1h", "--sort", sort]).map((i) => i.id);
  const byCreated = ids("created");
  expect(byCreated.indexOf(first)).toBeLessThan(byCreated.indexOf(second));
  const byUpdated = ids("updated");
  expect(byUpdated.indexOf(second)).toBeLessThan(byUpdated.indexOf(first));

  runCli(bin, ["signal", second, "--task", "approval", "--set", "approved=true"]);
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

  // Scoped to this test's process: the suite shares one server.
  const scope = ["--process", name, "--since", "1h"];
  const q = runCli(bin, ["instances", "-q", ...scope]);
  expect(q.ok).toBe(true);
  const lines = q.stdout.trim().split("\n");
  expect(lines.every((l) => ID_RE.test(l)), `not bare ids: ${q.stdout}`).toBe(true);
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
  // Else `genctl pause $(genctl instances -q ...)` receives "no" and "instances" as ids.
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

  // `genctl pause $(genctl instances -q --status running)`, narrowed to this test's ids: the
  // suite shares one server, and pausing every running instance stalls other tests.
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
  // Nested into `pause`, a notice on stdout would become an argument.
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
  await waitForExternalToken(id);

  expect(runCli(bin, ["pause", id]).ok).toBe(true);
  expect(instances(["--since", "1h"]).find((i) => i.id === id)?.status).toBe("paused");

  expect(runCli(bin, ["resume", id]).ok).toBe(true);
  expect(instances(["--since", "1h"]).find((i) => i.id === id)?.status).toBe("running");

  runCli(bin, ["signal", id, "--task", "approval", "--set", "approved=true"]);
  expect(await waitForInstance(id)).toBe("completed");
}, 30_000);

test("an id printed by one command is accepted by every other, verbatim", async () => {
  const name = apply(externalDef(uid("idform")));
  const listed = startedID(runCli(bin, ["run", name]).stdout);
  expect(listed).toMatch(ID_RE);

  const fromTable = instances(["--since", "1h"]).find((i) => i.id === listed);
  expect(fromTable, "the id the table lists is the id run printed").toBeDefined();

  // The logs ID column carries it in full, so a row can be copied straight into a command.
  const logged = runCli(bin, ["logs", listed, "--mode", "basic"]).stdout;
  expect(logged).toContain(listed);

  for (const args of [["get", listed], ["logs", listed], ["pause", listed], ["resume", listed]]) {
    const r = runCli(bin, args);
    expect(r.ok, `${args[0]} rejected the id it was given: ${r.stderr}`).toBe(true);
  }
}, 30_000);

// Unrefused, a child id would match no tree and report "unchanged", as if already stopped.
test("pause/resume/retry — a child is refused, naming the root to use instead", async () => {
  const child = uid("lchild");
  const parent = uid("lparent");
  runCli(bin, ["apply", "-f", writeDefs([externalDef(child), childDef(parent, child)])]);
  const rootID = startedID(runCli(bin, ["run", parent]).stdout);

  let kidID = "";
  for (let i = 0; i < 50 && !kidID; i++) {
    const kids = JSON.parse(
      runCli(bin, ["instances", "--process", child, "--children", "--since", "1h", "--json"]).stdout,
    );
    kidID = kids[0]?.id ?? "";
    if (!kidID) await new Promise((r) => setTimeout(r, 100));
  }
  expect(kidID, "the child instance never appeared").not.toBe("");

  for (const verb of ["pause", "resume", "cancel", "retry"]) {
    const r = runCli(bin, [verb, kidID]);
    expect(r.ok, `${verb} on a child should be refused`).toBe(false);
    expect(r.stderr).toContain("not a root instance");
    expect(r.stderr, "the refusal names the root to use instead").toContain(rootID);
  }

  // The root itself is accepted, so the refusal is about the child and not the tree.
  expect(runCli(bin, ["pause", rootID]).ok).toBe(true);
  expect(runCli(bin, ["resume", rootID]).ok).toBe(true);
}, 30_000);

test("cancel — stops an instance for good, and it stays stopped", async () => {
  const name = apply(externalDef(uid("cancellable")));
  const id = startedID(runCli(bin, ["run", name]).stdout);
  await waitForExternalToken(id);

  const r = runCli(bin, ["cancel", id]);
  expect(r.ok, `cancel failed: ${r.stderr}`).toBe(true);
  expect(r.stdout, "the CLI reports the verb it performed").toContain("cancelled");
  expect(instances(["--since", "1h"]).find((i) => i.id === id)?.status).toBe("cancelled");

  const resumed = runCli(bin, ["resume", id]);
  expect(resumed.ok, "a cancelled instance must not resume").toBe(false);
  expect(resumed.stderr).toContain("cancel");
  expect(resumed.stderr).toContain("new instance");
  expect(resumed.stderr, "retry refuses a cancel, so it must not be offered").not.toContain("retry it");
  expect(instances(["--since", "1h"]).find((i) => i.id === id)?.status).toBe("cancelled");

  const retried = runCli(bin, ["retry", id]);
  expect(retried.ok, "a cancelled instance must not be retryable").toBe(false);
  expect(retried.stderr).toContain("cancel");
}, 30_000);

// Re-running an assertion over a group of ids must converge. specs/id-list-commands.md.
test("cancel — a second cancel reports rather than failing", async () => {
  const name = apply(externalDef(uid("cancel_twice")));
  const id = startedID(runCli(bin, ["run", name]).stdout);
  await waitForExternalToken(id);

  expect(runCli(bin, ["cancel", id]).ok).toBe(true);
  const again = runCli(bin, ["cancel", id]);
  expect(again.ok, `a repeated cancel must not fail: ${again.stderr}`).toBe(true);
  expect(again.stdout).toContain("already");
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
// pause/resume are assertions: an id already in the asserted state is reported and forgiven,
// so a half-applied line can be re-run as-is. specs/id-list-commands.md.

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

  // The mistake this exists for: the TABLE as arguments (no -q), a real id among the words.
  const table = runCli(bin, ["instances", "--status", "running", "--since", "1h"]);
  const words = table.stdout.split(/\s+/).filter(Boolean);
  expect(words, "the table must carry both headers and a real id").toContain("STATUS");
  expect(words).toContain(id);

  const r = runCli(bin, ["pause", ...words]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("are not instance ids");
  expect(r.stderr).toContain("nothing was sent");
  // One line, not one per cell.
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

  // The shape check must not cost @last, which names an id rather than being one.
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

// ── @last ───────────────────────────────────────────────────────────────────────

test("@last — addresses the most recently started instance", () => {
  const name = apply(inputDef(uid("proc")));
  const id = runCli(bin, ["run", name, "--set", "count=7", "-q"]).stdout.trim();

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

test("instances --status — takes several, the same grammar upgrade --status takes", async () => {
  const done = startedID(runCli(bin, ["run", apply(switchDef(uid("st_done")))]).stdout);
  expect(await waitForInstance(done)).toBe("completed");
  const parked = startedID(runCli(bin, ["run", apply(externalDef(uid("st_parked")))]).stdout);
  await waitForExternalToken(parked);

  const both = instances(["--since", "1h", "--status", "completed,running"]).map((i) => i.id);
  expect(both).toContain(done);
  expect(both).toContain(parked);
  // A union, not the last value winning -- which is what a single-value filter would give.
  expect(instances(["--since", "1h", "--status", "completed"]).map((i) => i.id)).not.toContain(parked);

  runCli(bin, ["signal", parked, "--task", "approval", "--set", "approved=true"]);
  expect(await waitForInstance(parked)).toBe("completed");
}, 15_000);
