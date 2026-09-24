import { expect, test } from "vitest";
import { client, fetchObject, spliceObjects, waitForInstance } from "../helpers/client.ts";
import { waitForParked } from "../helpers/external.ts";
import { BASE_URL } from "../helpers/constants.ts";

// An instance is readable through two views and the split is deliberate.
//
//   GET /instances/{id}         what the instance reports OUTWARD: where it is, how it ended,
//                               the error it carries, and the `output:` its definition declared
//                               -- the same value a parent collects as a child's result.
//   GET /instances/{id}/detail  what it HOLDS: state exactly as stored, bookkeeping slots and
//                               all, plus the columns around it.
//
// State is engine-internal — it is what an upgrade validates and a migration rewrites — so it
// is not on the outward view. specs/version-compatibility.md.

async function completedInstance(): Promise<string> {
  const name = `views_${crypto.randomUUID().slice(0, 8)}`;
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      input_schema: { type: "object", properties: { n: { type: "number" } }, required: ["n"] },
      tasks: [{ id: "only", output: { v: "$: input.n" }, switch: "end" }],
      output: { v: "$: outputs.only.v" },
    },
  });
  expect(error).toBeUndefined();
  const { data } = await client.POST("/instances", { body: { process: name, input: { n: 1 } } });
  expect(await waitForInstance(data!.id)).toBe("completed");
  return data!.id;
}

test("the outward view carries no state — not under any name", async () => {
  const id = await completedInstance();
  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });

  // Asserted as the WHOLE key set: a field added here is a field added to the public surface,
  // and that should be a decision rather than a side effect.
  expect(Object.keys(data as object).sort()).toEqual([
    "created_at",
    "id",
    // The declared output: block, which is a projection of state rather than state -- a caller
    // reading it is reading what the process chose to report, not the engine's slots.
    "output",
    "process",
    "retry_count",
    "status",
    "task",
    "updated_at",
    "version",
  ]);
});

test("the detail view carries state, bookkeeping included", async () => {
  const id = await completedInstance();
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });

  const state = data!.state as Record<string, unknown>;
  // `last_error` is seeded null when the instance is created, so it is here even for a process with
  // no error handling at all -- a slot no definition declares, which the outward view used to
  // hide and this one must not. `output` is NOT here: a slot with a field of its own is moved
  // there, so nothing on this response is said twice.
  expect(Object.keys(state).sort()).toEqual(["input", "last_error", "outputs"]);
  expect(data!.output, "moved to its own field rather than dropped").toBeDefined();
  expect(data).toHaveProperty("task_epoch");
  expect(data).toHaveProperty("lease_epoch");
  expect(data).toHaveProperty("next_replayable");
});

// Config is resolved per tick from the environment and never persisted, which is what keeps
// secrets out of stored state. The fixture below is set on the test server.
test("a config value reaches expressions but neither view, nor the state behind them", async () => {
  const name = `views_secret_${crypto.randomUUID().slice(0, 8)}`;
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      config_schema: {
        type: "object",
        required: ["e2e_token"],
        properties: { e2e_token: { type: "string", secret: true } },
      },
      // The task READS config, so the value demonstrably reached the expression scope; what it
      // stores is a boolean about it, never the value itself.
      tasks: [{ id: "check", output: { ok: '$: config.e2e_token != ""' }, switch: "end" }],
      output: { ok: "$: outputs.check.ok" },
    },
  });
  expect(error).toBeUndefined();
  const { data: started } = await client.POST("/instances", { body: { process: name, input: {} } });
  expect(await waitForInstance(started!.id)).toBe("completed");

  for (const path of [`/instances/${started!.id}`, `/instances/${started!.id}/detail`]) {
    const body = await (await fetch(`${BASE_URL}/api${path}`)).text();
    expect(body, `${path} must not carry the config value`).not.toContain("supersecret-token-value");
    expect(body, `${path} must not carry a config slot at all`).not.toContain('"config"');
  }
});

// error_data is the one value on the outward view that has no size limit -- a clause may attach
// anything. Past the inline cutoff it must behave like every other externalized slot: ABSENT
// from the data and listed instead, never a {ref, size} marker sitting where the value goes.
// It is not resolved server-side: that would put an unbounded response behind no control at all.
test("an oversized error_data is listed, not inlined and not leaked as a marker", async () => {
  const name = `views_bigdata_${crypto.randomUUID().slice(0, 8)}`;
  const blob = "B".repeat(8 * 1024);
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "go",
          switch: [{ panic: { code: "boom", message: "kaboom", data: { blob, small: "inline" } } }],
        },
      ],
    },
  });
  expect(error).toBeUndefined();
  const { data: started } = await client.POST("/instances", { body: { process: name, input: {} } });
  expect(await waitForInstance(started!.id)).toBe("failed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id: started!.id } } });
  const payload = data!.error_data as Record<string, unknown>;
  expect(payload.small, "a small sibling is not dragged out with the big leaf").toBe("inline");
  expect(payload.blob, "the oversized leaf is absent, not a marker").toBeUndefined();
  expect(JSON.stringify(payload), "no reference may sit where a value goes").not.toContain("ref");

  const listed = (data!.objects ?? []).find(
    (o) => o.path?.[0] === "error_data" && o.path?.[1] === "blob",
  );
  expect(listed, "past the cutoff it must be listed at the path it belongs to").toBeDefined();
  // fetchObject returns the stored JSON text, so the string arrives quoted.
  expect(JSON.parse(await fetchObject(listed!.ref))).toBe(blob);
});

// `output` is the other unbounded value on the outward view -- a definition may declare any
// shape -- so it obeys the same rule error_data does, and the paths it is listed under are
// rooted at THIS response rather than at the state slot it came from. A path naming `state`
// here would send a caller looking for a field the outward view does not have.
test("an oversized output is listed at its own path, not inlined and not leaked as a marker", async () => {
  const name = `views_bigout_${crypto.randomUUID().slice(0, 8)}`;
  const blob = "B".repeat(8 * 1024);
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: { blob: { type: "string" } },
        required: ["blob"],
      },
      tasks: [{ id: "pass", output: { echo: "$: input.blob" }, switch: "end" }],
      output: { echo: "$: outputs.pass.echo", small: "inline" },
    },
  });
  expect(error).toBeUndefined();
  const { data: started } = await client.POST("/instances", {
    body: { process: name, input: { blob } },
  });
  expect(await waitForInstance(started!.id)).toBe("completed");
  const id = started!.id;

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  const output = data!.output as Record<string, unknown>;
  expect(output.small, "a small sibling is not dragged out with the big leaf").toBe("inline");
  expect(output.echo, "the oversized leaf is absent, not a marker").toBeUndefined();
  expect(JSON.stringify(output), "no reference may sit where a value goes").not.toContain("ref");

  const listed = (data!.objects ?? []).find((o) => o.path?.[1] === "echo");
  expect(listed, "past the cutoff it must be listed").toBeDefined();
  expect(
    listed!.path,
    "rooted at the field on THIS response -- `state` names nothing the outward view returns",
  ).toEqual(["output", "echo"]);
  expect(JSON.parse(await fetchObject(listed!.ref))).toBe(blob);

  // detail holds the same value under the same field, so it is listed at the same path -- and
  // at that path ONLY, because the slot was moved out of `state` rather than copied beside it.
  const { data: detail } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  const paths = (detail!.objects ?? []).map((o) => JSON.stringify(o.path));
  expect(paths).toContain(JSON.stringify(["output", "echo"]));
  expect(paths, "a value listed twice is a value a caller can splice into a place it is not")
    .not.toContain(JSON.stringify(["state", "output", "echo"]));
});

// The detail view is a strict SUPERSET of the status one: every field the status endpoint
// returns, detail returns too, so moving a caller to it can never lose them a field. Asserted
// across the shapes whose OPTIONAL fields differ -- a completed instance has no error, a failed
// one has all three parts of it, a parked one has a phase -- because a superset that only
// holds for the fields present on a happy path is not one.
test("the detail view returns every field the status view does", async () => {
  const cases: Record<string, string> = {};

  cases.completed = await completedInstance();

  const panicking = `views_super_panic_${crypto.randomUUID().slice(0, 8)}`;
  await client.PUT("/definitions", {
    body: {
      name: panicking,
      tasks: [
        { id: "go", switch: [{ panic: { code: "boom", message: "kaboom", data: { why: "x" } } }] },
      ],
    },
  });
  const { data: failed } = await client.POST("/instances", { body: { process: panicking, input: {} } });
  expect(await waitForInstance(failed!.id)).toBe("failed");
  cases.failed = failed!.id;

  const parking = `views_super_park_${crypto.randomUUID().slice(0, 8)}`;
  await client.PUT("/definitions", {
    body: {
      name: parking,
      tasks: [
        {
          id: "hold",
          action: { type: "external", input: {}, result_schema: {} },
          output: { r: "$: self.result" },
          switch: "end",
        },
      ],
    },
  });
  const { data: parked } = await client.POST("/instances", { body: { process: parking, input: {} } });
  cases.parked = parked!.id;
  // Parked, not settled: waitForInstance waits for a terminal status and this one never reaches
  // it, so wait on the phase the case is actually about.
  for (let i = 0; i < 100; i++) {
    const { data } = await client.GET("/instances/{id}", { params: { path: { id: parked!.id } } });
    if (data?.phase === "external") break;
    await new Promise((r) => setTimeout(r, 50));
  }

  for (const [label, id] of Object.entries(cases)) {
    const { data: status } = await client.GET("/instances/{id}", { params: { path: { id } } });
    const { data: detail } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
    const missing = Object.keys(status as object).filter((k) => !(k in (detail as object)));
    expect(missing, `${label}: detail must carry every field the status view returns`).toEqual([]);
  }
});

// State is RECONSTRUCTIBLE from the wire: what /detail returns plus what it lists is the whole
// of it. That is the contract behind listing rather than inlining -- a caller fetches the pieces
// and puts them back at the paths given, and ends up holding exactly what the instance holds.
//
// Checked against the server's OWN reconstruction (?resolve=true) as well as against the value
// that went in, so a listing that is complete but mis-pathed fails, and so does one where both
// routes agree on the same wrong answer.
test("state can be rebuilt from what the detail view lists", async () => {
  const name = `views_rebuild_${crypto.randomUUID().slice(0, 8)}`;
  const blob = "R".repeat(4 * 1024);
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: { blob: { type: "string" }, small: { type: "number" } },
        required: ["blob"],
      },
      tasks: [{ id: "only", output: { echo: "$: input.blob" }, switch: "end" }],
      output: { echo: "$: outputs.only.echo" },
    },
  });
  expect(error).toBeUndefined();
  const { data: started } = await client.POST("/instances", {
    body: { process: name, input: { blob, small: 7 } },
  });
  expect(await waitForInstance(started!.id)).toBe("completed");
  const id = started!.id;

  const { data: listed } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  expect(listed!.objects?.length, "the fixture must externalize, or this proves nothing").toBeGreaterThan(0);
  expect((listed!.state as any).input.blob, "an externalized slot is absent, not a marker").toBeUndefined();

  const rebuilt = await spliceObjects(structuredClone(listed));
  expect((rebuilt!.state as any).input.blob).toBe(blob);
  expect((rebuilt!.state as any).input.small, "an inline sibling is untouched by the rebuild").toBe(7);

  const { data: server } = await client.GET("/instances/{id}/detail", {
    params: { path: { id }, query: { resolve: true } },
  });
  expect(server!.state, "the caller's rebuild and the server's must agree").toEqual(rebuilt!.state);
});

// `external_input` is the third unbounded value on the outward view, and the only one that
// describes what an instance WANTS rather than what it produced. It is on this view because a
// parked instance's request is outward-facing: reading it takes no claim, so an operator can see
// what is being asked without leasing the task away from a worker. The full work contract
// (result_schema, raises) stays with the CLAIM. specs/external-task-queue.md.
async function parkedOnExternal(extra: Record<string, unknown> = {}): Promise<{ id: string; name: string }> {
  const name = `views_ext_${crypto.randomUUID().slice(0, 8)}`;
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      input_schema: { type: "object", properties: { amount: { type: "number" } }, required: ["amount"] },
      tasks: [
        {
          id: "review",
          action: {
            type: "external",
            // Deliberately NOT the process input: a transformation of it, so a test cannot pass
            // by surfacing the wrong slot under the right name.
            input: { doubled: "$: input.amount * 2", note: "please review", ...extra },
            result_schema: { type: "object", properties: { ok: { type: "boolean" } }, required: ["ok"] },
          },
          output: "$: self.result",
          switch: "end",
        },
      ],
      output: { ok: "$: outputs.review.ok" },
    },
  });
  expect(error).toBeUndefined();
  const { data } = await client.POST("/instances", { body: { process: name, input: { amount: 21 } } });
  await waitForParked(data!.id);
  return { id: data!.id, name };
}

test("the outward view carries the parked external input, under a name of its own", async () => {
  const { id } = await parkedOnExternal();
  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });

  // The evaluated snapshot, not the process's own input -- which is why the name is not `input`.
  expect(data!.external_input).toEqual({ doubled: 42, note: "please review" });

  // The whole key set again, for the parked shape: this view gained a field and that must stay a
  // decision. `state` in particular must not have arrived with it.
  expect(Object.keys(data as object).sort()).toEqual([
    "created_at",
    "external_input",
    "id",
    "phase",
    "process",
    "retry_count",
    "status",
    "task",
    "updated_at",
    "version",
  ]);
});

test("the parked external input is gone once the task is answered", async () => {
  const { id } = await parkedOnExternal();
  const { token } = await waitForParked(id);
  const { error } = await client.POST("/external-tasks/resolve", { body: { token, result: { ok: true } } });
  expect(error).toBeUndefined();
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  // The engine deletes the slot when it consumes the answer, so absence here is "not parked"
  // and needs no phase check on the read side.
  expect(data!.external_input, "a settled instance is not asking for anything").toBeUndefined();
  expect(data!.output).toEqual({ ok: true });
});

test("an oversized external input is listed at its own path, not inlined and not leaked as a marker", async () => {
  const blob = "B".repeat(8 * 1024);
  const { id } = await parkedOnExternal({ blob });

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  const asked = data!.external_input as Record<string, unknown>;
  expect(asked.note, "a small sibling is not dragged out with the big leaf").toBe("please review");
  expect(asked.blob, "the oversized leaf is absent, not a marker").toBeUndefined();
  expect(JSON.stringify(asked), "no reference may sit where a value goes").not.toContain("ref");

  // Rooted at the RESPONSE field, which the state slot is now spelled the same as -- the
  // outward view has no state for a path to point into.
  const listed = (data!.objects ?? []).find(
    (o) => o.path?.[0] === "external_input" && o.path?.[1] === "blob",
  );
  expect(listed, "past the cutoff it must be listed at the path it belongs to").toBeDefined();
  expect(JSON.parse(await fetchObject(listed!.ref))).toBe(blob);
});

// detail moves three state slots to fields of their own, so the same value is never in two
// places on one response -- and its objects entry names one path, not two. Listed twice, a
// caller splicing the listing writes the value into a slot the response does not have; listed
// only under `state`, the field beside it is left short a leaf with nothing pointing at it.
test("detail says each value once, and lists it at the field that holds it", async () => {
  const blob = "M".repeat(8 * 1024);
  const { id } = await parkedOnExternal({ blob });

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  expect(Object.keys(data!.state as object), "a slot with a field of its own is moved out of state")
    .not.toContain("external_input");

  const paths = (data!.objects ?? []).map((o) => (o.path ?? []).join("."));
  expect(paths).toContain("external_input.blob");
  expect(paths, "the slot is not under state any more, so nothing may be listed there")
    .not.toContain("state.external_input.blob");
  expect(new Set(paths).size, "one value, one entry").toBe(paths.length);

  // And the field is spliceable from that single entry.
  const rebuilt = await spliceObjects(structuredClone(data));
  expect((rebuilt!.external_input as Record<string, unknown>).blob).toBe(blob);
});
