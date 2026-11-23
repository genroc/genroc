import { expect, test } from "vitest";
import { client } from "../helpers/client.ts";

const validDef = {
  name: `test_def_${crypto.randomUUID()}`,

  tasks: [
    {
      id: "step1",
      action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/action", timeout: 1000 },
      switch: [{ goto: "end" }],
    },
  ],
};

test("PUT /definitions — registers a new definition", async () => {
  const { data, error } = await client.PUT("/definitions", { body: { definitions: [validDef] } });

  expect(error).toBeUndefined();
  expect(data?.[0].name).toBe(validDef.name);
});

test("GET /definitions — lists registered definitions", async () => {
  await client.PUT("/definitions", { body: { definitions: [validDef] } });

  // The list is paginated, so page through (following page.after) to find the
  // freshly registered definition rather than assuming it's on the first page.
  let found = false;
  let after: string | undefined;
  do {
    const { data, error } = await client.GET("/definitions", {
      params: { query: { limit: 1000, after } },
    });
    expect(error).toBeUndefined();
    if ((data!.items ?? []).some((d) => d.name === validDef.name)) {
      found = true;
      break;
    }
    after = data!.page.after || undefined;
  } while (after);
  expect(found).toBe(true);
});

test("PUT /definitions — rejects rest call without endpoint", async () => {
  const { data, error } = await client.PUT("/definitions", {
    body: { definitions: [{
      name: "bad",
      tasks: [
        {
          id: "s1",
          action: { type: "fetch" as const } as any,
          switch: [{ goto: "end" }],
        },
      ],
    }] },
  });

  expect(error).toBeDefined();
  expect(data).toBeUndefined();
});

test("PUT /definitions — rejects unknown call type", async () => {
  const { data, error } = await client.PUT("/definitions", {
    body: { definitions: [{
      name: "bad",
      tasks: [
        {
          id: "s1",
          action: { type: "ftp", url: "x" } as any,
          switch: [{ goto: "end" }],
        },
      ],
    }] },
  });

  expect(error).toBeDefined();
  expect(data).toBeUndefined();
});

test("PUT /definitions — rejects unknown task type", async () => {
  const { data, error } = await client.PUT("/definitions", {
    body: { definitions: [{
      name: "bad",
      tasks: [{ type: "parallel", id: "p1" } as any],
    }] },
  });

  expect(error).toBeDefined();
  expect(data).toBeUndefined();
});

test("PUT /definitions — rejects schema default that violates its own schema", async () => {
  const { data, error } = await client.PUT("/definitions", {
    body: { definitions: [{
      name: "bad",
      input_schema: {
        type: "object",
        properties: { blob: { type: "string", default: 12 } },
      },
      tasks: [
        {
          id: "s1",
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/action" },
          switch: [{ goto: "end" }],
        },
      ],
    }] },
  });

  expect(data).toBeUndefined();
  expect((error as any)?.error).toContain("default does not validate");
});

test("PUT /definitions — rejects missing process name", async () => {
  const { data, error } = await client.PUT("/definitions", {
    body: { definitions: [{
      tasks: [
        {
          id: "s1",
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/action" },
          switch: [{ goto: "end" }],
        },
      ],
    } as any] },
  });

  expect(error).toBeDefined();
  expect(data).toBeUndefined();
});

// An empty `-` entry decodes to a null task, which every pre-validation walk over `tasks` dereferenced.
test("PUT /definitions — a null task entry is rejected, not a crash", async () => {
  const { data, error } = await client.PUT("/definitions", {
    body: {
      definitions: [{ name: `null_task_${crypto.randomUUID()}`, tasks: [null] }],
    } as any,
  });

  expect(data).toBeUndefined();
  expect((error as any)?.code).toBe("invalid");
  expect((error as any)?.error).toContain("tasks[0] is empty");
});

// ── POST /definitions/validate: a dry-run apply ────────────────────────────────

async function dryRun(definitions: object[], channel?: string) {
  return client.POST("/definitions/validate", { body: { channel, definitions } as never });
}

async function apply(definitions: object[], channel?: string) {
  return client.PUT("/definitions", { body: { channel, definitions } as never });
}

const switchOnly = (name: string, extra: object = {}) => ({
  name,
  tasks: [{ id: "route", switch: "end" }],
  ...extra,
});

test("POST /definitions/validate — answers what the apply then does, and writes nothing", async () => {
  const name = `dryrun_${crypto.randomUUID()}`;
  await apply([switchOnly(name)], "latest");
  const changed = switchOnly(name, { output: { ok: true } });

  const dry = await dryRun([changed], "staging");
  expect(dry.error).toBeUndefined();

  const real = await apply([changed], "staging");
  expect(real.error).toBeUndefined();
  // Had the dry run written, this apply would find v2 stored (saved: false) or staging moved.
  expect(dry.data, "the dry run's answer is the apply's, so it wrote nothing").toEqual(real.data);
  expect(real.data).toEqual([{ name, version: 2, saved: true, previous: 0, same_runtime: false }]);
});

// Without it a client applying several definitions cannot tell which one an address belongs to:
// `""` and a shared task id are in every one of them, and a definition may have no name.
test("POST /definitions/validate — each field gives the index of the definition it is in", async () => {
  const ok = `named_ok_${crypto.randomUUID().slice(0, 8)}`;
  const orphan = `named_orphan_${crypto.randomUUID().slice(0, 8)}`;
  const delay = (forValue: unknown) => ({ id: "a", action: { type: "delay", for: forValue }, switch: "end" });
  const { error } = await dryRun([
    { name: ok, tasks: [delay(10)] },
    { name: orphan, tasks: [delay(10), { ...delay(10), id: "orphan" }] },
  ]);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const fields = (error as any)?.fields;
  expect(fields, JSON.stringify(error)).toEqual([expect.objectContaining({ definition_index: 1, field: "" })]);
});

test("POST /definitions/validate — a definition with no name is still placed, by its index", async () => {
  const nameless = { tasks: [{ id: "route", switch: "end" }] };
  const { error } = await dryRun([switchOnly(`nameless_ok_${crypto.randomUUID().slice(0, 8)}`), nameless]);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const body = error as any;
  expect(body?.fields, JSON.stringify(error)).toEqual([
    expect.objectContaining({ definition_index: 1, field: "name", rule: "required" }),
  ]);
  expect(body?.error, "the prose has no name to lead with either").toBe("definitions[1]: name is required");
});

test("PUT /definitions — every definition that does not decode is named, and nothing is stored", async () => {
  const ok = `decode_ok_${crypto.randomUUID().slice(0, 8)}`;
  const { error } = await apply([
    { ...switchOnly(`decode_a_${crypto.randomUUID().slice(0, 8)}`), tasks: [{ id: "a", swtich: "end" }] },
    switchOnly(ok),
    { ...switchOnly(`decode_c_${crypto.randomUUID().slice(0, 8)}`), tasks: "a" },
  ]);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const fields = (error as any)?.fields;
  expect(fields, "one strict decode of the whole body stopped at the first, and named no definition").toEqual([
    { definition_index: 0, field: "", rule: "decode", message: 'json: unknown field "swtich"' },
    expect.objectContaining({ definition_index: 2, field: "", rule: "decode" }),
  ]);
  const stored = await client.GET("/definitions/{name}", { params: { path: { name: ok } } });
  expect(stored.response.status, "a decode failure refuses the whole batch").toBe(404);
});

test("POST /definitions/validate — a child that is not there is placed at its name", async () => {
  const missing = `missing_child_${crypto.randomUUID().slice(0, 8)}`;
  const { error } = await dryRun([{
    name: `missing_parent_${crypto.randomUUID().slice(0, 8)}`,
    tasks: [{ id: "a", action: { type: "child_map", children: { k: { name: missing } } }, switch: "end" }],
  }]);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((error as any)?.fields, JSON.stringify(error)).toEqual([expect.objectContaining({
    definition_index: 0,
    field: "tasks.a.action.children.k.name",
    message: expect.stringContaining(`task "a" child ${missing}["k"]: not on channel "latest"`),
  })]);
});

test("POST /definitions/validate — a rule written by hand is placed at the slot it names", async () => {
  const { error } = await dryRun([{
    name: `hand_rule_${crypto.randomUUID().slice(0, 8)}`,
    tasks: [{ id: "a", switch: [{ goto: "end", raise: { code: "x" } }] }],
  }]);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((error as any)?.fields, "only struct-tag rules were placed; the rest came back as prose").toEqual([{
    definition_index: 0,
    field: "tasks.a.switch.0",
    rule: "def.structure",
    message: 'task "a" switch case 0: set exactly one of "goto", "raise", "panic"',
  }]);
});

test("POST /definitions/validate — every definition that fails its own checks is reported", async () => {
  const { error } = await dryRun([
    { name: `empty_a_${crypto.randomUUID().slice(0, 8)}`, tasks: [] },
    switchOnly(`empty_ok_${crypto.randomUUID().slice(0, 8)}`),
    { name: `empty_c_${crypto.randomUUID().slice(0, 8)}`, tasks: [] },
  ]);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const fields = (error as any)?.fields;
  expect(fields?.map((f: { definition_index: number }) => f.definition_index), "not only the first").toEqual([0, 2]);
});

test("POST /definitions/compat — a submitted definition that does not decode is named by its index", async () => {
  const { error } = await client.POST("/definitions/compat", {
    body: {
      from: { channel: "latest" },
      to: { definitions: [switchOnly(`compat_ok_${crypto.randomUUID().slice(0, 8)}`), { name: "x", tasks: [{ id: "a", swtich: "end" }] }] },
    } as never,
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((error as any)?.fields, JSON.stringify(error)).toEqual([
    { definition_index: 1, field: "", rule: "decode", message: 'json: unknown field "swtich"' },
  ]);
});

test("PUT /definitions — two definitions with one name are refused, rather than one dropped", async () => {
  const name = `twice_${crypto.randomUUID().slice(0, 8)}`;
  const { error } = await apply([
    { name, tasks: [{ id: "a", action: { type: "delay", for: "x" }, switch: "end" }] },
    switchOnly(`twice_other_${crypto.randomUUID().slice(0, 8)}`),
    { name, tasks: [{ id: "a", action: { type: "delay", for: 10 }, switch: "end" }] },
  ]);
  const message = `definition "${name}" appears twice in this apply`;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const body = error as any;
  expect(body?.error, "the first was skipped in silence, invalid as it was").toBe(message);
  expect(body?.fields, "each copy is placed, by its own index").toEqual([
    { definition_index: 0, field: "name", rule: "unique", message },
    { definition_index: 2, field: "name", rule: "unique", message },
  ]);
  const stored = await client.GET("/definitions/{name}", { params: { path: { name } } });
  expect(stored.response.status, "nothing of the batch was stored").toBe(404);
});

test("POST /definitions/validate — refuses exactly what apply refuses", async () => {
  const child = `dryrun_child_${crypto.randomUUID()}`;
  await apply([switchOnly(child)], "latest");

  const cases: Array<[string, object[], string, string]> = [
    [
      "a required config var unset on the server",
      [switchOnly(`dryrun_cfg_${crypto.randomUUID()}`, {
        config_schema: { type: "object", required: ["e2e_not_set"], properties: { e2e_not_set: { type: "string" } } },
      })],
      "latest",
      "e2e_not_set",
    ],
    [
      "an unpinned child that exists but is not on the requested channel",
      [{
        name: `dryrun_parent_${crypto.randomUUID()}`,
        tasks: [{ id: "spawn", action: { type: "child", name: child }, switch: "end" }],
      }],
      "staging",
      "not on channel",
    ],
  ];
  for (const [why, defs, channel, want] of cases) {
    const real = await apply(defs, channel);
    expect(JSON.stringify(real.error), `apply refuses ${why}`).toContain(want);
    const dry = await dryRun(defs, channel);
    expect(JSON.stringify(dry.error), `a dry run passing ${why} would promise an apply that fails`).toContain(want);
  }
});
