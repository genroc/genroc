import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";
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
  // output_order is the engine's own: no definition declares it, and it is exactly the kind of
  // slot the outward view used to hide and this one must not. `error` is seeded null when the
  // instance is created, so it is here even for a process with no error handling at all.
  expect(Object.keys(state).sort()).toEqual(["error", "input", "output", "output_order", "outputs"]);
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
