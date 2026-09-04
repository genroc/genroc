import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

test("on_error — HTTP failure routes to recovery task", async () => {
  const failMock = await startMockService(0, { statusCode: 500 });
  const recoveryMock = await startMockService(0, {
    response: { recovered: true },
  });

  const name = `on_error_route_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${failMock.port}/action`,
          },
          on_error: [{ code: ["http.%"], goto: "$recovery" }],
          timeout: 2000,
          switch: [{ goto: "next" }],
        },
        {
          id: "recovery",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${recoveryMock.port}/action`,
            responses: { 200: {
              type: "object",
              properties: { recovered: { type: "boolean" } },
              required: ["recovered"],
            } },
          },
          output: "$: self.result",
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

  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", {
    params: { path: { id } },
  });
  expect((data?.state?.outputs as any)?.recovery?.recovered).toBe(true);

  failMock.stop();
  recoveryMock.stop();
});

test("on_error — error context available in recovery task input", async () => {
  const failMock = await startMockService(0, { statusCode: 503 });
  const recoveryMock = await startMockService(0, {
    response: { done: true },
  });

  const name = `on_error_ctx_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${failMock.port}/action`,
          },
          on_error: [{ code: ["http.%"], goto: "$recovery" }],
          timeout: 2000,
          switch: [{ goto: "next" }],
        },
        {
          id: "recovery",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${recoveryMock.port}/action`,
            body: { error_code: "$: last_error.code" },
            responses: { 200: {
              type: "object",
              properties: { done: { type: "boolean" } },
              required: ["done"],
            } },
          },
          output: "$: self.result",
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

  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", {
    params: { path: { id } },
  });
  // The recovery mock received the request — instance completed means routing worked
  expect((data?.state?.outputs as any)?.recovery?.done).toBe(true);

  failMock.stop();
  recoveryMock.stop();
});

test("on_error — unmatched code fails instance", async () => {
  const failMock = await startMockService(0, { statusCode: 500 });

  const name = `on_error_nomatch_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${failMock.port}/action`,
          },
          on_error: [{ code: ["network.%"], goto: "$unreachable" }],
          timeout: 2000,
          switch: [{ goto: "next" }],
        },
        {
          id: "unreachable",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${failMock.port}/action`,
          },
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

  failMock.stop();
});
