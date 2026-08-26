import { expect, test } from "vitest";
import { client, fetchObject, spliceObjects, waitForInstance } from "../helpers/client.ts";
import { BASE_URL } from "../helpers/constants.ts";

// An instance is readable through two views and the split is deliberate.
//
//   GET /instances/{id}         what the instance reports OUTWARD: where it is, how it ended,
//                               and the error it carries.
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
  // `error` is seeded null when the instance is created, so it is here even for a process with
  // no error handling at all -- a slot no definition declares, which the outward view used to
  // hide and this one must not.
  expect(Object.keys(state).sort()).toEqual(["error", "input", "output", "outputs"]);
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
    const body = await (await fetch(`${BASE_URL}${path}`)).text();
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

// The detail view is a strict SUPERSET of the status one: every field the status endpoint
// returns, detail returns too, so moving a caller to it can never lose them a field. Asserted
// across the shapes whose OPTIONAL fields differ -- a completed instance has no error, a failed
// one has all three parts of it, a parked one has a wait_state -- because a superset that only
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
  // it, so wait on the wait_state the case is actually about.
  for (let i = 0; i < 100; i++) {
    const { data } = await client.GET("/instances/{id}", { params: { path: { id: parked!.id } } });
    if (data?.wait_state === "external") break;
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
