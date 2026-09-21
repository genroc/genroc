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
            method: "post",
            url: `http://localhost:${failMock.port}/action`,
            timeout: 2000,
          },
          on_error: [{ code: ["http.%"], goto: "$recovery" }],
          switch: [{ goto: "next" }],
        },
        {
          id: "recovery",
          action: {
            type: "fetch" as const,
            method: "post",
            url: `http://localhost:${recoveryMock.port}/action`,
            responses: { 200: {
              type: "object",
              properties: { recovered: { type: "boolean" } },
              required: ["recovered"],
            } },
            timeout: 2000,
          },
          output: "$: self.result",
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
            method: "post",
            url: `http://localhost:${failMock.port}/action`,
            timeout: 2000,
          },
          on_error: [{ code: ["http.%"], goto: "$recovery" }],
          switch: [{ goto: "next" }],
        },
        {
          id: "recovery",
          action: {
            type: "fetch" as const,
            method: "post",
            url: `http://localhost:${recoveryMock.port}/action`,
            body: { error_code: "$: last_error.code" },
            responses: { 200: {
              type: "object",
              properties: { done: { type: "boolean" } },
              required: ["done"],
            } },
            timeout: 2000,
          },
          output: "$: self.result",
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
            method: "post",
            url: `http://localhost:${failMock.port}/action`,
            timeout: 2000,
          },
          // Reachable but not matching: the mock answers 500. An impossible code would be
          // refused at registration now, so "no rule matched" has to be a runtime miss.
          on_error: [{ code: ["http.404"], goto: "$unreachable" }],
          switch: [{ goto: "next" }],
        },
        {
          id: "unreachable",
          action: {
            type: "fetch" as const,
            method: "post",
            url: `http://localhost:${failMock.port}/action`,
            timeout: 500,
          },
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

// `retries` counts the EXTRA attempts, so 3 retries is 4 requests in all — a promise only a
// request count can keep, and one an off-by-one in the budget would silently break. The goto
// is what proves the budget was spent rather than the instance still parked.
// docs guides/process-definition/error-handling.mdx.
test("retry — N retries is N+1 requests, then the goto", async () => {
  const failMock = await startMockService(0, { statusCode: 500 });

  const name = `retry_budget_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch" as const,
            method: "post",
            url: `http://localhost:${failMock.port}/action`,
            timeout: 2000,
          },
          on_error: [{ code: ["http.500"], retry: { retries: 3, delay: 10 }, goto: "$gave_up" }],
          switch: [{ goto: "end" }],
        },
        { id: "gave_up", output: { gave_up: true }, switch: [{ goto: "end" }] },
      ],
    },
  });

  const { data: startData } = await client.POST("/instances", { body: { process: name } });
  const id = startData!.id;
  expect(await waitForInstance(id, 15_000)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  expect((data?.state?.outputs as any)?.gave_up?.gave_up, "the goto runs once the budget is spent").toBe(true);
  expect(failMock.requestCount(), "3 retries must be the first attempt plus 3, not 3 in total").toBe(4);

  failMock.stop();
});
