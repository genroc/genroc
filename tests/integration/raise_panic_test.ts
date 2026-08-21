import { expect, test } from "vitest";
import {
  client,
  startMockService,
  waitForInstance,
} from "../helpers/client.ts";

// The two authored terminal clauses and the column that tells them apart.
//
// `raise` and `panic` write identical fields and differ only in status, which is the
// whole point: status is what decides whether ancestors are poisoned and whether the
// process is retryable. These tests pin that difference, plus the error_code column
// being populated for every non-success outcome — including the engine's own failures,
// where the code used to exist only inside the error prose.

// A raise concludes the process. It is not a failure, so it is not retryable, and the
// rejection has to say why rather than just "not retryable".
test("raise — switch case concludes the process as raised, with its code", async () => {
  const name = `raise_switch_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: { funds: { type: "integer" } },
        required: ["funds"],
      },
      tasks: [
        {
          id: "check",
          switch: [
            {
              case: "input.funds < 100",
              raise: {
                code: "insufficient_funds",
                message: "the account has insufficient funds",
              },
            },
            { goto: "end" },
          ],
        },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: name, input: { funds: 5 } },
  });
  const id = started!.id;

  expect(await waitForInstance(id)).toBe("raised");

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(data?.status).toBe("raised");
  expect(data?.error_code).toBe("insufficient_funds");
  expect(data?.error).toBe("the account has insufficient funds");

  // A raise is a declared outcome, not a fault: retry must refuse it, and say so in
  // terms an operator can act on instead of a bare "not retryable".
  const { error } = await client.POST("/instances/{id}/retry", {
    params: { path: { id } },
  });
  const msg = JSON.stringify(error);
  expect(msg).toContain("insufficient_funds");
  expect(msg).toContain("declared outcome");
});

// The same clause, reached through on_error rather than a switch — the other of the
// two places a Fault can live.
test("raise — on_error rule raises instead of routing", async () => {
  const failMock = await startMockService(0, { statusCode: 402 });

  const name = `raise_onerror_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "charge",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${failMock.port}/charge`,
          },
          on_error: [
            {
              code: ["http.402"],
              raise: {
                code: "card_declined",
                message: "the issuer declined the charge",
              },
            },
          ],
          timeout: 2000,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: name },
  });
  const id = started!.id;

  expect(await waitForInstance(id)).toBe("raised");

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(data?.error_code).toBe("card_declined");
  expect(data?.error).toBe("the issuer declined the charge");
  // The engine's own code stays visible in `error`, so the underlying cause is not lost
  // when error_code becomes the authored one.
  expect((data?.context?.error as Record<string, unknown>)?.code).toBe(
    "http.402",
  );

  failMock.stop();
});

// A panic is a defect the author detected. It produces `failed`, so unlike a raise it
// stays retryable — authoring it grants no special status.
test("panic — fails the process with the authored code, and stays retryable", async () => {
  const name = `panic_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "guard",
          switch: [
            {
              case: "true",
              panic: {
                code: "submit_contract_violation",
                message: "the service returned 200 with an error body",
              },
            },
            { goto: "end" },
          ],
        },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: name },
  });
  const id = started!.id;

  expect(await waitForInstance(id)).toBe("failed");

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(data?.status).toBe("failed");
  expect(data?.error_code).toBe("submit_contract_violation");
  // The authored message replaces the engine's generic reason — that is the only
  // observable difference from any other defect.
  expect(data?.error).toBe("the service returned 200 with an error body");

  // Retry accepts it: `panic` chose `failed` precisely because it means "this is a
  // fault", and faults are what retry is for.
  const { error } = await client.POST("/instances/{id}/retry", {
    params: { path: { id } },
  });
  expect(error).toBeUndefined();
});

// `panic` on an on_error rule (not just a switch case): the same authored defect,
// reached through the action-failure path. This exercises handleCallError's panic branch.
test("panic — an on_error rule panics on an action task", async () => {
  const failMock = await startMockService(0, { statusCode: 500 });

  const name = `panic_onerror_${crypto.randomUUID()}`;
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
          on_error: [
            {
              code: ["http.5%"],
              panic: {
                code: "upstream_contract_broken",
                message: "the upstream returned an unusable 5xx",
              },
            },
          ],
          timeout: 2000,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: name },
  });
  const id = started!.id;

  expect(await waitForInstance(id)).toBe("failed");

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(data?.status).toBe("failed");
  expect(data?.error_code).toBe("upstream_contract_broken");
  expect(data?.error).toBe("the upstream returned an unusable 5xx");
  // The engine's own code stays in `error` underneath the authored one.
  expect((data?.context?.error as Record<string, unknown>)?.code).toBe(
    "http.500",
  );

  failMock.stop();
});

// on_error → end now computes the process output, exactly like a normal completion. All
// three end paths (normal, on_error, batch-resolution) share one helper, so a process
// caught by an on_error goto:end produces its output instead of silently dropping it.
test("on_error → end computes the process output, like a normal completion", async () => {
  const failMock = await startMockService(0, { statusCode: 404 });

  const name = `onerror_end_output_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      // Static output (present on both the normal-end and error-end terminals): before
      // the fix an on_error → end completion left it unset; now it is computed.
      output: '$: "recovered"',
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${failMock.port}/action`,
          },
          on_error: [{ code: ["http.404"], goto: "end" }],
          timeout: 2000,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: name },
  });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(data?.status).toBe("completed");
  expect(data?.context?.output).toBe("recovered");

  failMock.stop();
});

// §7.1's real payoff: an engine-detected failure is queryable by code too, not just
// authored ones. Before this the code lived only inside the error text.
test("error_code — engine failures carry their own dotted code", async () => {
  const failMock = await startMockService(0, { statusCode: 500 });

  const name = `engine_code_${crypto.randomUUID()}`;
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
          timeout: 2000,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: name },
  });
  const id = started!.id;

  expect(await waitForInstance(id)).toBe("failed");

  const { data } = await client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(data?.error_code).toBe("http.500");

  failMock.stop();
});

// The column exists to be filtered on; a code that is never filterable is just prose.
test("error_code — is a list filter, and raised is a status filter", async () => {
  const name = `raise_filter_${crypto.randomUUID()}`;
  const code = `filter_probe_${crypto.randomUUID().slice(0, 8)}`.replace(
    /-/g,
    "_",
  );
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "check",
          switch: [
            { case: "true", raise: { code, message: "a probe raise" } },
            { goto: "end" },
          ],
        },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: name },
  });
  await waitForInstance(started!.id);

  const { data: byCode } = await client.GET("/instances", {
    params: { query: { error_code: code } },
  });
  const byCodeItems = byCode!.items ?? [];
  expect(byCodeItems.map((i) => i.id)).toEqual([started!.id]);
  expect(byCodeItems[0]?.status).toBe("raised");

  const { data: byStatus } = await client.GET("/instances", {
    params: { query: { status: "raised", error_code: code } },
  });
  expect((byStatus!.items ?? []).map((i) => i.id)).toEqual([started!.id]);
});

// The raise set is derived, not authored, and published so "what can this process
// raise?" is answerable without reading the file. Panic codes are excluded because no
// on_error rule can ever match one.
test("raises — the derived set is published, and excludes panic codes", async () => {
  const name = `raises_set_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "check",
          switch: [
            { case: "1 == 2", raise: { code: "zebra_case", message: "z" } },
            { case: "1 == 3", raise: { code: "alpha_case", message: "a" } },
            {
              case: "1 == 4",
              panic: { code: "broken_contract", message: "b" },
            },
            { goto: "end" },
          ],
        },
      ],
    },
  });

  // The shared test DB holds far more than one page of definitions, so page through
  // by cursor until this one turns up rather than assuming it lands on page 1.
  let entry: { name: string; raises?: string[] } | undefined;
  let after: string | undefined;
  for (let page = 0; page < 50 && !entry; page++) {
    const { data } = await client.GET("/definitions", {
      params: { query: { limit: 100, after } },
    });
    entry = (data!.items ?? []).find((d) => d.name === name);
    if (!data!.page.after) break;
    after = data!.page.after;
  }
  // Sorted, deduped, panic-free.
  expect(entry?.raises).toEqual(["alpha_case", "zebra_case"]);
});

// ── the message is a template ───────────────────────────────────────────────────
//
// A raise message is rendered against the scope its clause fires in, so a failure can name
// the value that caused it. The CODE stays a literal — it is what makes the raise set
// computable and error_code filterable — while the message is prose nothing branches on.
// It must be a string: it lands in inst.Error, the audit log, and a parent's error.message.

test("raise message — interpolates the scope the clause fires in", async () => {
  const name = `raise_msg_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: { amount: { type: "integer" }, who: { type: "string" } },
        required: ["amount", "who"],
      },
      tasks: [
        {
          id: "check",
          switch: [
            {
              case: "input.amount > 100",
              raise: {
                code: "limit_exceeded",
                message: "${input.who} asked for ${input.amount}, over the limit",
              },
            },
            { goto: "end" },
          ],
        },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: name, input: { amount: 250, who: "sam" } },
  });
  const id = started!.id!;
  expect(await waitForInstance(id)).toBe("raised");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  expect(data!.error).toBe("sam asked for 250, over the limit");
  // The code is untouched by rendering — an operator still filters on the literal.
  expect(data!.error_code).toBe("limit_exceeded");
});

test("raise message — $${ is an escape, so a literal ${ survives", async () => {
  const name = `raise_esc_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "check",
          switch: [{ raise: { code: "syntax_help", message: "write $${x} to escape" } }],
        },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", {
    body: { process: name, input: {} },
  });
  const id = started!.id!;
  expect(await waitForInstance(id)).toBe("raised");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  expect(data!.error).toBe("write ${x} to escape");
});

test("raise message — a non-string message is refused at registration", async () => {
  const name = `raise_nonstr_${crypto.randomUUID()}`;
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: { amount: { type: "integer" } },
        required: ["amount"],
      },
      tasks: [
        {
          id: "check",
          // A bare `$:` leaf keeps its own type — unlike `${ }`, which stringifies — so
          // this is a number where prose is required.
          switch: [{ raise: { code: "too_big", message: "$: input.amount" } }],
        },
      ],
    },
  });
  expect(JSON.stringify(error)).toMatch(/message must be a string/);
});

test("raise message — a possibly-null message is refused, with the fix named", async () => {
  const name = `raise_null_${crypto.randomUUID()}`;
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      input_schema: { type: "object", properties: { who: { type: "string" } } },
      tasks: [
        {
          id: "check",
          switch: [{ raise: { code: "unknown_who", message: "$: input.who" } }],
        },
      ],
    },
  });
  expect(JSON.stringify(error)).toMatch(/may be null/);
  expect(JSON.stringify(error)).toMatch(/\?\?/);
});

test("raise message — an on_error rule reads the error it caught", async () => {
  const svc = await startMockService(0, { statusCode: 500, response: { why: "overloaded" } });
  try {
    const name = `raise_onerr_${crypto.randomUUID()}`;
    await client.PUT("/definitions", {
      body: {
        name,
        tasks: [
          {
            id: "call",
            action: {
              type: "fetch",
              url: `http://localhost:${svc.port}/boom`,
              responses: {
                "200": {},
                "500": {
                  type: "object",
                  properties: { why: { type: "string" } },
                  required: ["why"],
                },
              },
            },
            timeout: "5s",
            on_error: [
              {
                code: ["http.500"],
                raise: { code: "upstream_down", message: "upstream said ${error.data.why}" },
              },
            ],
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: started } = await client.POST("/instances", {
      body: { process: name, input: {} },
    });
    const id = started!.id!;
    expect(await waitForInstance(id)).toBe("raised");

    const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
    expect(data!.error).toBe("upstream said overloaded");
  } finally {
    await svc.stop();
  }
});
