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
  const { data, error } = await client.PUT("/definitions", { body: validDef });

  expect(error).toBeUndefined();
  expect(data?.name).toBe(validDef.name);
});

test("GET /definitions — lists registered definitions", async () => {
  await client.PUT("/definitions", { body: validDef });

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
    body: {
      name: "bad",
      tasks: [
        {
          id: "s1",
          action: { type: "fetch" as const } as any,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  expect(error).toBeDefined();
  expect(data).toBeUndefined();
});

test("PUT /definitions — rejects unknown call type", async () => {
  const { data, error } = await client.PUT("/definitions", {
    body: {
      name: "bad",
      tasks: [
        {
          id: "s1",
          action: { type: "ftp", url: "x" } as any,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  expect(error).toBeDefined();
  expect(data).toBeUndefined();
});

test("PUT /definitions — rejects unknown task type", async () => {
  const { data, error } = await client.PUT("/definitions", {
    body: {
      name: "bad",
      tasks: [{ type: "parallel", id: "p1" } as any],
    },
  });

  expect(error).toBeDefined();
  expect(data).toBeUndefined();
});

test("PUT /definitions — rejects schema default that violates its own schema", async () => {
  const { data, error } = await client.PUT("/definitions", {
    body: {
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
    },
  });

  expect(data).toBeUndefined();
  expect((error as any)?.error).toContain("default does not validate");
});

test("PUT /definitions — rejects missing process name", async () => {
  const { data, error } = await client.PUT("/definitions", {
    body: {
      tasks: [
        {
          id: "s1",
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/action" },
          switch: [{ goto: "end" }],
        },
      ],
    } as any,
  });

  expect(error).toBeDefined();
  expect(data).toBeUndefined();
});

// An empty `-` entry decodes to a null task, which every pre-validation walk over `tasks` dereferenced.
test("PUT /definitions/batch — a null task entry is rejected, not a crash", async () => {
  const { data, error } = await client.PUT("/definitions/batch", {
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

async function batch(definitions: object[], channel?: string) {
  return client.PUT("/definitions/batch", { body: { channel, definitions } as never });
}

const switchOnly = (name: string, extra: object = {}) => ({
  name,
  tasks: [{ id: "route", switch: "end" }],
  ...extra,
});

test("POST /definitions/validate — answers what the apply then does, and writes nothing", async () => {
  const name = `dryrun_${crypto.randomUUID()}`;
  await batch([switchOnly(name)], "latest");
  const changed = switchOnly(name, { output: { ok: true } });

  const dry = await dryRun([changed], "staging");
  expect(dry.error).toBeUndefined();

  const real = await batch([changed], "staging");
  expect(real.error).toBeUndefined();
  // Had the dry run written, this apply would find v2 stored (saved: false) or staging moved.
  expect(dry.data, "the dry run's answer is the apply's, so it wrote nothing").toEqual(real.data);
  expect(real.data).toEqual([{ name, version: 2, saved: true, previous: 0 }]);
});

test("POST /definitions/validate — refuses exactly what apply refuses", async () => {
  const child = `dryrun_child_${crypto.randomUUID()}`;
  await batch([switchOnly(child)], "latest");

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
    const real = await batch(defs, channel);
    expect(JSON.stringify(real.error), `apply refuses ${why}`).toContain(want);
    const dry = await dryRun(defs, channel);
    expect(JSON.stringify(dry.error), `a dry run passing ${why} would promise an apply that fails`).toContain(want);
  }
});
