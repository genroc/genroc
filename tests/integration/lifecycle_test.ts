import { expect, test } from "vitest";
import {
  client,
  startMockService,
  waitForInstance,
} from "../helpers/client.ts";

test("lifecycle — task task completes when service returns ok", async () => {
  const mock = await startMockService(0, {
    response: { done: true },
  });

  const name = `lifecycle_ok_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${mock.port}/action`,
            result_schema: { type: "object", properties: { done: { type: "boolean" } } },
          },
          output: "$: self.result",
          timeout: 2000,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data: startData } = await client.POST("/instances", {
    body: { process: name, input: { x: 1 } },
  });
  const id = startData!.id;

  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect((data?.context?.outputs as any)?.call?.done).toBe(true);

  mock.stop();
});

test("lifecycle — task task fails and marks failed", async () => {
  const mock = await startMockService(0, { statusCode: 500 });

  const name = `lifecycle_fail_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "call",
          action: { type: "fetch" as const, url: `http://localhost:${mock.port}/action` },
          timeout: 500,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data: startData } = await client.POST("/instances", {
    body: { process: name },
  });
  expect(await waitForInstance(startData!.id, 10_000)).toBe("failed");

  mock.stop();
});

test("lifecycle — conditional routes to correct branch", async () => {
  const thenMock = await startMockService(0, {
    response: { branch: "then" },
  });
  const elseMock = await startMockService(0, {
    response: { branch: "else" },
  });

  const name = `lifecycle_cond_${crypto.randomUUID()}`;
  const def = await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: {
          go_then: { type: "boolean" },
        },
        required: ["go_then"],
      },
      tasks: [
        {
          id: "start",
          action: { type: "fetch" as const, url: `http://localhost:${thenMock.port}/action` },
          switch: [{ case: "input.go_then", goto: "$then_task" }, { goto: "$else_task" }],
        },
        {
          id: "then_task",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${thenMock.port}/action`,
            result_schema: { type: "object", properties: { branch: { type: "string" } } },
          },
          output: "$: self.result",
          timeout: 1000,
          switch: [{ goto: "end" }],
        },
        {
          id: "else_task",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${elseMock.port}/action`,
            result_schema: { type: "object", properties: { branch: { type: "string" } } },
          },
          output: "$: self.result",
          timeout: 1000,
          switch: [{ goto: "end" }],
        },
      ],
    },
  } as const);

  expect(def.error).toBeUndefined();

  let i1Create = await client.POST("/instances", {
    body: { process: name, input: { go_then: true } },
  });
  expect(i1Create.error).toBeUndefined();
  await waitForInstance(i1Create.data!.id);

  const i1 = await client.GET("/instances/{id}", {
    params: { path: { id: i1Create.data!.id } },
  });

  expect(i1.error).toBeUndefined();

  expect((i1.data?.context?.outputs as any)?.then_task?.branch).toBe("then");
  expect((i1.data?.context?.outputs as any)?.else_task?.branch).toBe(undefined);

  const i2Create = await client.POST("/instances", {
    body: { process: name, input: { go_then: false } },
  });
  expect(i2Create.error).toBeUndefined();

  await waitForInstance(i2Create.data!.id);
  const i2 = await client.GET("/instances/{id}", {
    params: { path: { id: i2Create.data!.id } },
  });
  expect((i2.data?.context?.outputs as any)?.else_task?.branch).toBe("else");
  expect((i2.data?.context?.outputs as any)?.then_task?.branch).toBe(undefined);

  thenMock.stop();
  elseMock.stop();
});

test("lifecycle — task fails when output violates result_schema", async () => {
  const mock = await startMockService(0, {
    response: { wrong_field: true },
  });

  const name = `lifecycle_result_schema_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "charge",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${mock.port}/action`,
            result_schema: {
              type: "object",
              properties: { charged: { type: "boolean" } },
              required: ["charged"],
            },
          },
          timeout: 2000,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data: startData } = await client.POST("/instances", {
    body: { process: name },
  });
  const id = startData!.id;

  expect(await waitForInstance(id, 5000)).toBe("failed");

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(data!.error!).toContain("output");

  mock.stop();
});
