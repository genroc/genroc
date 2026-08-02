import { expect, test } from "vitest";
import { client } from "../helpers/client.ts";

const validDef = {
  name: `test_def_${crypto.randomUUID()}`,

  tasks: [
    {
      id: "step1",
      action: { type: "fetch" as const, url: "http://localhost:19990/action" },
      timeout: 1000,
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
          action: { type: "fetch" as const, url: "http://localhost:19990/action" },
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
          action: { type: "fetch" as const, url: "http://localhost:19990/action" },
          switch: [{ goto: "end" }],
        },
      ],
    } as any,
  });

  expect(error).toBeDefined();
  expect(data).toBeUndefined();
});
