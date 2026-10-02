import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// ── Static validation ─────────────────────────────────────────────────────────

// `case` is a legal on_error key (M2), but a CODE LIST under it is an author reaching for `code`,
// and the rejection must name the key they meant.
test("on_error — a code list under \"case\" is rejected by name", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_wrong_key_${crypto.randomUUID()}`,
      tasks: [
        {
          id: "charge",
          only_once: true,
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/x" },
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
          on_error: [{ case: ["pre.4%"], not_reached: true, retry: 3, goto: "end" } as any],
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(error?.error).toContain(`"case" is a boolean expression, not a list`);
  expect(error?.error).toContain(`select errors by code with "code"`);
});

test("switch — an on_error's \"code\" key is rejected by name", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_wrong_key_switch_${crypto.randomUUID()}`,
      tasks: [
        {
          id: "charge",
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/x" },
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
          switch: [{ code: ["http.500"], goto: "end" } as any],
        },
      ],
    },
  });
  expect(error?.error).toContain(`unknown field "code"`);
  expect(error?.error).toContain(`a switch case selects with "case"`);
});

test("only_once:true — rejects retries on http.% pattern", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_http_${crypto.randomUUID()}`,
      tasks: [
        {
          id: "charge",
          only_once: true,
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/x" },
          on_error: [{ code: ["http.%"], retry: 3 }],
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(error).toBeDefined();
  expect(JSON.stringify(error)).toContain("http.%");
});

test("only_once:true — rejects retries on exact http.500", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_exact_${crypto.randomUUID()}`,
      tasks: [
        {
          id: "charge",
          only_once: true,
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/x" },
          on_error: [{ code: ["http.500"], retry: 1 }],
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(error).toBeDefined();
  expect(JSON.stringify(error)).toContain("http.500");
});

test("only_once:true — rejects catch-all with retries", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_catchall_${crypto.randomUUID()}`,
      tasks: [
        {
          id: "charge",
          only_once: true,
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/x" },
          on_error: [{ retry: 2 }],
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(error).toBeDefined();
  expect(JSON.stringify(error)).toContain("catch-all");
});

test("only_once:true — rejects wildcard crossing namespaces", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_cross_${crypto.randomUUID()}`,
      tasks: [
        {
          id: "charge",
          only_once: true,
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/x" },
          on_error: [{ code: ["s%"], retry: 3 }],
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(error).toBeDefined();
  expect(JSON.stringify(error)).toContain("s%");
});

test("only_once:true — accepts retries on pre.%", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_start_${crypto.randomUUID()}`,
      tasks: [
        {
          id: "charge",
          only_once: true,
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/x" },
          on_error: [
            { code: ["pre.%"], retry: 3 },
            { goto: "end" },
          ],
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(error).toBeUndefined();
});

test("only_once:true — accepts retries on exact pre.* codes", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_exact_start_${crypto.randomUUID()}`,
      tasks: [
        {
          id: "charge",
          only_once: true,
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/x" },
          on_error: [{ code: ["pre.error", "pre.timeout"], retry: 3 }],
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(error).toBeUndefined();
});

test("only_once:true — accepts not_reached:true override for http.422", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_exec_false_${crypto.randomUUID()}`,
      tasks: [
        {
          id: "charge",
          only_once: true,
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/x" },
          on_error: [
            { code: ["http.422"], not_reached: true, retry: 2 },
            { code: ["http.%"], goto: "end" },
          ],
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(error).toBeUndefined();
});

// A catch-all also matches codes where nothing came back (http.timeout, external.timeout,
// only_once.interrupted), so not_reached cannot license its retries.
test("only_once:true — not_reached:true does not rescue a catch-all with retries", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_catchall_exec_${crypto.randomUUID()}`,
      tasks: [
        {
          id: "charge",
          only_once: true,
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/x" },
          on_error: [{ not_reached: true, retry: 2 }],
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(error?.error).toContain(
    "catch-all rule cannot have retries on an only_once task",
  );
});

// The message must say not_reached cannot go through a wildcard, not name whichever code it reached.
test("only_once:true — not_reached:true cannot be asserted through a wildcard", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_timeout_exec_${crypto.randomUUID()}`,
      tasks: [
        {
          id: "charge",
          only_once: true,
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/x" },
          on_error: [{ code: ["http.%"], not_reached: true, retry: 2 }],
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(error?.error).toContain(`pattern "http.%" cannot be a wildcard`);
  expect(error?.error).toContain("name the exact codes instead");
});

// The codes nothing came back from cannot be excepted at all, and saying so is more use
// than repeating the wildcard advice that would lead nowhere.
test("only_once:true — an unknowable code cannot be retried even when named exactly", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_unknowable_${crypto.randomUUID()}`,
      tasks: [
        {
          id: "charge",
          only_once: true,
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/x" },
          on_error: [{ code: ["http.timeout"], not_reached: true, retry: 2 }],
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(error?.error).toContain("http.timeout can never be retried on an only_once task");
  expect(error?.error).toContain("check the system of record instead");
});

// The shape the rules push authors towards, so it must keep working.
test("only_once:true — exact codes asserted with not_reached:true are retryable", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_exact_exec_${crypto.randomUUID()}`,
      tasks: [
        {
          id: "charge",
          only_once: true,
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/x" },
          on_error: [
            { code: ["pre.%"], retry: 3 },
            { code: ["http.409", "http.422"], not_reached: true, retry: 2 },
          ],
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(error).toBeUndefined();
});

// Catching an unknowable code is always legal — that is the whole recovery path. Only
// retrying it is refused.
test("only_once:true — routing only_once.interrupted without retries is accepted", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_interrupted_route_${crypto.randomUUID()}`,
      tasks: [
        {
          id: "charge",
          only_once: true,
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/x" },
          on_error: [
            {
              code: ["only_once.interrupted", "http.timeout"],
              goto: "$verify",
            },
          ],
          switch: [{ goto: "end" }],
        },
        {
          id: "verify",
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/x" },
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(error).toBeUndefined();
});

test("only_once:true — next-only rule on http.% is accepted (no retries)", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `ni_static_goto_only_${crypto.randomUUID()}`,
      tasks: [
        {
          id: "charge",
          only_once: true,
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/x" },
          on_error: [{ code: ["http.%"], goto: "$handler" }],
          switch: [{ goto: "next" }],
        },
        {
          id: "handler",
          action: { type: "fetch" as const, method: "post", url: "http://localhost:19990/x" },
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(error).toBeUndefined();
});

// ── Runtime behaviour ─────────────────────────────────────────────────────────

test("only_once:true — http.500 routes to handler and is called exactly once", async () => {
  const failMock = await startMockService(0, { statusCode: 500 });
  const handlerMock = await startMockService(0, { response: { handled: true } });

  const name = `ni_rt_no_retry_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "charge",
          only_once: true,
          action: {
            type: "fetch" as const,
            method: "post",
            url: `http://localhost:${failMock.port}/action`,
            timeout: 2000,
          },
          on_error: [
            // pre.% rule present — would retry on connection errors but not on http.*
            { code: ["pre.%"], retry: 3 },
            { code: ["http.%"], goto: "$handler" },
          ],
          switch: [{ goto: "next" }],
        },
        {
          id: "handler",
          action: {
            type: "fetch" as const,
            method: "post",
            url: `http://localhost:${handlerMock.port}/action`,
            responses: { 200: {
              type: "object",
              properties: { handled: { type: "boolean" } },
              required: ["handled"],
            } },
            timeout: 2000,
          },
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data } = await client.POST("/instances", { body: { process: name } });
  expect(await waitForInstance(data!.id)).toBe("completed");

  // The key assertion: only one call to the failing endpoint — no retries fired
  expect(failMock.requestCount()).toBe(1);
  expect(handlerMock.requestCount()).toBe(1);

  failMock.stop();
  handlerMock.stop();
});

test("only_once:true — connection refused triggers pre.% retries", async () => {
  // Start then immediately stop the mock to free the port — subsequent connects will be refused
  const gone = await startMockService(0);
  const port = gone.port;
  await gone.stop();

  const name = `ni_rt_start_retry_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "charge",
          only_once: true,
          action: {
            type: "fetch" as const,
            method: "post",
            url: `http://localhost:${port}/action`,
            timeout: 2000,
          },
          on_error: [
            // 1 retry on pre.% then complete via end
            { code: ["pre.%"], retry: 1, goto: "end" },
          ],
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data } = await client.POST("/instances", { body: { process: name } });
  // Two attempts (original + 1 retry), both refused. Retries exhausted → next: end → completed.
  // The 2-second retry delay means this takes ~2s — well within the 30s test timeout.
  expect(await waitForInstance(data!.id, 15_000)).toBe("completed");
});

test("only_once:true — not_reached:true allows retry on http.422", async () => {
  const mock = await startMockService(0, { statusCode: 200, response: { ok: true } });
  // The mock cannot vary status per call, so this only confirms the definition is accepted and
  // runs; the static acceptance test above is the meaningful check.

  const name = `ni_rt_exec_false_${crypto.randomUUID()}`;
  const { error: defErr } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "charge",
          only_once: true,
          action: {
            type: "fetch" as const,
            method: "post",
            url: `http://localhost:${mock.port}/action`,
            responses: { 200: {
              type: "object",
              properties: { ok: { type: "boolean" } },
              required: ["ok"],
            } },
            timeout: 2000,
          },
          on_error: [{ code: ["http.422"], not_reached: true, retry: 2 }],
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(defErr).toBeUndefined();

  const { data } = await client.POST("/instances", { body: { process: name } });
  expect(await waitForInstance(data!.id)).toBe("completed");

  mock.stop();
});

test("default task (no only_once) — http.500 retries normally", async () => {
  // Baseline without only_once: original + 1 retry, then end.
  const failMock = await startMockService(0, { statusCode: 500 });

  const name = `default_retry_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "call",
          // No only_once:true — default behaviour
          action: {
            type: "fetch" as const,
            method: "post",
            url: `http://localhost:${failMock.port}/action`,
            timeout: 2000,
          },
          on_error: [{ code: ["http.%"], retry: 1, goto: "end" }],
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data } = await client.POST("/instances", { body: { process: name } });
  // 1 retry = 2s delay; allow up to 15s
  expect(await waitForInstance(data!.id, 15_000)).toBe("completed");

  expect(failMock.requestCount()).toBe(2);

  failMock.stop();
});
