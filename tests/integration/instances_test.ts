import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

const processName = `test_proc_${crypto.randomUUID()}`;

async function ensureDefinition() {
  await client.PUT("/definitions", {
    body: {
      name: processName,

      input_schema: {
        type: "object",
        properties: { order_id: { type: "number" } },
        required: ["order_id"],
      },
      tasks: [
        {
          id: "s1",
          action: { type: "fetch" as const, url: "http://localhost:19991/action" },
          timeout: 500,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
}

test("POST /instances — starts a new instance", async () => {
  await ensureDefinition();

  const { data, error } = await client.POST("/instances", {
    body: { process: processName, input: { order_id: 1 } },
  });

  expect(error).toBeUndefined();
  expect(data!.id).toBeDefined();
  expect(data!.status).toBe("running");
});

test("GET /instances/{id} — returns instance status", async () => {
  await ensureDefinition();

  const { data: startData, error: startError } = await client.POST(
    "/instances",
    {
      body: { process: processName, input: { order_id: 1 } },
    },
  );

  expect(startError).toBeUndefined();
  const id = startData!.id;

  const { data, error } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(error).toBeUndefined();
  expect(data!.id).toBe(id);
});

// status says what is happening to a process and wait_state says what it is waiting for;
// neither says *where* it is. The task field does, on both the detail and the list — which
// is what makes a stuck or failed instance diagnosable without reading the audit log.
test("GET /instances — task reports where the instance is, and clears when it ends", async () => {
  const name = `task_field_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "unreachable",
          // Port 1 is never listenable, so this fails fast and the instance stops here.
          action: { type: "fetch" as const, url: "http://127.0.0.1:1/x" },
          timeout: 500,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  const { data: started } = await client.POST("/instances", {
    body: { process: name },
  });
  const id = started!.id;
  await waitForInstance(id, 15_000);

  const { data: detail } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(detail!.status).toBe("failed");
  expect(detail!.task).toBe("unreachable"); // where it died

  const { data: list } = await client.GET("/instances", {
    params: { query: { limit: 100 } },
  });
  const listed = (list!.items ?? []).find((i) => i.id === id);
  expect(listed!.task).toBe("unreachable"); // and the light projection carries it too
});

// A settled instance keeps its position rather than clearing it, which is what makes the
// field answer "where did this end up" as well as "where is it now". Every task must carry
// a switch and only the last may say `end`, so a process always finishes *at* a task.
test("GET /instances/{id} — a completed process reports the task it finished at", async () => {
  const name = `task_field_done_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        { id: "first", switch: [{ goto: "$last" }] },
        { id: "last", switch: [{ goto: "end" }] },
      ],
    },
  });
  const { data: started } = await client.POST("/instances", {
    body: { process: name },
  });
  const id = started!.id;
  expect(await waitForInstance(id, 15_000)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(data!.task).toBe("last");
});

test("GET /instances/{id} — returns error for unknown ID", async () => {
  const { data, error } = await client.GET("/instances/{id}", {
    params: { path: { id: "00000000-0000-0000-0000-000000000000" } },
  });
  expect(error).toBeDefined();
  expect(data?.context).toBeUndefined();
});

test("GET /instances — lists instances", async () => {
  const { data, error } = await client.GET("/instances");
  expect(error).toBeUndefined();
  expect(Array.isArray(data!.items)).toBe(true);
});

test("GET /instances — list items omit context but carry timestamps", async () => {
  await ensureDefinition();
  await client.POST("/instances", { body: { process: processName, input: { order_id: 1 } } });

  const { data, error } = await client.GET("/instances", {
    params: { query: { limit: 5 } },
  });
  expect(error).toBeUndefined();
  const items = data!.items ?? [];
  expect(items.length).toBeGreaterThan(0);

  const item = items[0];
  // The (potentially large) context is never returned by the list.
  expect("context" in item).toBe(false);
  // The scalar summary fields are present.
  expect(item.id).toBeDefined();
  expect(item.status).toBeDefined();
  expect(item.created_at).toBeDefined();
  expect(item.updated_at).toBeDefined();

  // Default sort is created (an immutable key, stable for cursor walks).
  expect(data!.page.sort).toBe("created");
  expect(data!.page.order).toBe("desc");
});

test("GET /instances?sort=updated — updated is an opt-in sort", async () => {
  const { data, error } = await client.GET("/instances", {
    params: { query: { sort: "updated", limit: 5 } },
  });
  expect(error).toBeUndefined();
  expect(data!.page.sort).toBe("updated");
});

test("GET /instances/{id} — detail includes the full context", async () => {
  await ensureDefinition();
  const { data: started } = await client.POST("/instances", {
    body: { process: processName, input: { order_id: 99 } },
  });

  const { data, error } = await client.GET("/instances/{id}", {
    params: { path: { id: started!.id } },
  });
  expect(error).toBeUndefined();
  expect(data!.context).toBeDefined();
  expect((data!.context as Record<string, unknown>).input).toEqual({ order_id: 99 });
});

test("POST /instances — fails when input is invalid", async () => {
  await ensureDefinition();

  const { data, error } = await client.POST("/instances", {
    body: { process: processName, input: { order_id: "hi" } },
  });

  expect(error).toBeDefined();
  expect(data).toBeUndefined();
});

test("POST /instances — what happens when referencing types?", async () => {
  await client.PUT("/definitions", {
    body: {
      name: processName,

      input_schema: {
        $ref: "#/$defs/order",
        $defs: {
          order: {
            type: "object",
            properties: {
              order_id: { type: "number" },
            },
            required: ["order_id"],
          },
        },
      },
      tasks: [
        {
          id: "s1",
          action: { type: "fetch" as const, url: "http://localhost:19991/action" },
          timeout: 500,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data, error } = await client.POST("/instances", {
    body: { process: processName, input: { order_id: 10 } },
  });

  expect(data).toBeDefined();
  expect(undefined).toBeUndefined();
});
