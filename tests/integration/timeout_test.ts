import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// A fetch whose mock holds the first response for 3s. Every timeout below is far under
// that and far under the 30s default, so the task erroring at all is the proof that the
// authored deadline — not the default — is what bounded the call.
const HELD_MS = 3_000;

async function runWithTimeout(timeout: unknown) {
  const name = `timeout_${crypto.randomUUID()}`;
  const mock = await startMockService(0, { response: { ok: true }, firstRequestDelayMs: HELD_MS });
  try {
    const { error: defErr } = await client.PUT("/definitions", {
      body: {
        name,
        tasks: [
          {
            id: "call",
            action: { type: "fetch" as const, url: `http://localhost:${mock.port}/action` },
            timeout,
            on_error: [{ code: ["http.timeout"], goto: "$handled" }],
            switch: [{ goto: "end" }],
          },
          { id: "handled", switch: [{ raise: { code: "timed_out", message: "deadline fired" } }] },
        ],
      } as never,
    });
    if (defErr) throw new Error(`define failed: ${JSON.stringify(defErr)}`);

    const { data } = await client.POST("/instances", { body: { process: name } });
    const id = data!.id;
    const status = await waitForInstance(id, 15_000);
    return { status, id };
  } finally {
    await mock.stop();
  }
}

test("duration-string timeout bounds a fetch", async () => {
  const { status } = await runWithTimeout("300ms");
  // 'raised' is only reachable through the on_error route, so it distinguishes a fired
  // timeout from a mock that simply answered — 'completed' would be both.
  expect(status, "a 300ms timeout must fire well before the mock's 3s response").toBe("raised");
});

test("object-form timeout bounds a fetch", async () => {
  const { status } = await runWithTimeout({ for: "300ms" });
  expect(status, "the {for} object form must bound the call exactly as the shorthand does").toBe(
    "raised",
  );
});

test("expression timeout bounds a fetch", async () => {
  const name = `timeout_expr_${crypto.randomUUID()}`;
  const mock = await startMockService(0, { response: { ok: true }, firstRequestDelayMs: HELD_MS });
  try {
    await client.PUT("/definitions", {
      body: {
        name,
        input_schema: {
          type: "object",
          properties: { budget_ms: { type: "integer" } },
          required: ["budget_ms"],
        },
        tasks: [
          {
            id: "call",
            action: { type: "fetch" as const, url: `http://localhost:${mock.port}/action` },
            timeout: "$: input.budget_ms",
            on_error: [{ code: ["http.timeout"], goto: "$handled" }],
            switch: [{ goto: "end" }],
          },
          { id: "handled", switch: [{ raise: { code: "timed_out", message: "deadline fired" } }] },
        ],
      } as never,
    });

    const { data } = await client.POST("/instances", {
      body: { process: name, input: { budget_ms: 300 } },
    });
    expect(
      await waitForInstance(data!.id, 15_000),
      "a caller-supplied budget must bound the call — this is what a static int could not do",
    ).toBe("raised");
  } finally {
    await mock.stop();
  }
});

// The slot `until` exists for: a deadline that is an instant rather than a budget. Passed as
// unix ms from the caller, so the deadline is a fixed point in time regardless of when the
// engine reaches the task.
test("until deadline bounds an external task", async () => {
  const name = `timeout_until_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: { deadline_ms: { type: "integer" } },
        required: ["deadline_ms"],
      },
      tasks: [
        {
          id: "review",
          action: { type: "external" as const },
          timeout: { until: "$: input.deadline_ms" },
          on_error: [{ code: ["external.timeout"], goto: "$expired" }],
          switch: [{ goto: "end" }],
        },
        { id: "expired", switch: [{ raise: { code: "timed_out", message: "deadline fired" } }] },
      ],
    } as never,
  });

  const { data } = await client.POST("/instances", {
    body: { process: name, input: { deadline_ms: Date.now() + 1_000 } },
  });
  expect(
    await waitForInstance(data!.id, 20_000),
    "nobody resolves the task, so the until deadline must expire it into external.timeout",
  ).toBe("raised");
});

test("until is rejected on a fetch task", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `timeout_until_fetch_${crypto.randomUUID()}`,
      tasks: [
        {
          id: "call",
          action: { type: "fetch" as const, url: "http://localhost:1/action" },
          timeout: { until: "fri 17:00" },
          switch: [{ goto: "end" }],
        },
      ],
    } as never,
  });
  expect(
    JSON.stringify(error ?? {}),
    "a fetch deadline already past would report http.timeout for a request never sent",
  ).toContain("only valid on an external task");
});

test("a timeout that resolves into the past fails the instance rather than expiring it", async () => {
  const name = `timeout_past_${crypto.randomUUID()}`;
  const mock = await startMockService(0, { response: { ok: true } });
  try {
    await client.PUT("/definitions", {
      body: {
        name,
        input_schema: {
          type: "object",
          properties: { budget_ms: { type: "integer" } },
          required: ["budget_ms"],
        },
        tasks: [
          {
            id: "call",
            action: { type: "fetch" as const, url: `http://localhost:${mock.port}/action` },
            timeout: "$: input.budget_ms",
            // A catch-all: the point is that no on_error rule can rescue this, because the
            // failure is the definition's, not the call's.
            on_error: [{ goto: "$handled" }],
            switch: [{ goto: "end" }],
          },
          { id: "handled", switch: [{ goto: "end" }] },
        ],
      } as never,
    });

    const { data } = await client.POST("/instances", {
      body: { process: name, input: { budget_ms: 0 } },
    });
    expect(
      await waitForInstance(data!.id, 15_000),
      "a zero budget must fail loudly, not silently report a timeout for a call never made",
    ).toBe("failed");
    expect(mock.requestCount(), "the request must never have been sent").toBe(0);
  } finally {
    await mock.stop();
  }
});
