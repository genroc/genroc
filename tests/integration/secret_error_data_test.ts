import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// `secret: true` inside a DECLARED payload — a fetch's error body or a child's raise data.
// Both land in error.data, which the context schema had no slot for at all, so the marker was
// invisible and the value reached the API and the audit trail in the clear.

const PAN = "4111111111111111";

test("a secret in a declared raise payload is redacted from the API context and the logs", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `sec_raise_child_${uid}`;
  await client.PUT("/definitions", {
    body: {
      name: child,
      tasks: [
        {
          id: "charge",
          switch: [
            { case: "true", raise: { code: "card_declined", message: "no", data: { pan: PAN, why: "51" } } },
            { goto: "end" },
          ],
        },
      ],
    },
  });

  const parent = `sec_raise_parent_${uid}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "pay",
          action: {
            type: "child" as const,
            name: child,
            raises: {
              card_declined: {
                type: "object",
                properties: { pan: { type: "string", secret: true }, why: { type: "string" } },
                required: ["pan", "why"],
              },
            },
          },
          on_error: [{ code: ["card_declined"], goto: "$carry" }],
          switch: [{ goto: "end" }],
        },
        // The message interpolates the secret, which is what puts it in the audit trail.
        {
          id: "carry",
          output: { seen: "$: error.data" },
          switch: [
            { case: "true", raise: { code: "gave_up", message: "declined ${error.data.pan}" } },
            { goto: "end" },
          ],
        },
      ],
    } as never,
  });
  expect(putErr).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: parent } });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("raised");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  const seen = (data?.context?.outputs as any)?.carry?.seen;
  expect(seen?.pan, "the declaration marked it secret").toBe("***");
  expect(seen?.why, "its plain sibling is untouched").toBe("51");
  // The audit trail is scrubbed by VALUE, not by schema: the rendered message interpolated
  // the secret, and it is collected from error.data before any line is written.
  const { data: logs } = await client.GET("/instances/{id}/logs", { params: { path: { id } } });
  expect(JSON.stringify(logs), "a message that interpolated it must not store it").not.toContain(PAN);
});

// The same fix on the channel that shipped first: a secret declared for a status body.
test("a secret in a declared fetch error body is redacted too", async () => {
  const failing = await startMockService(0, { statusCode: 404, response: { pan: PAN, why: "gone" } });
  const name = `sec_fetch_${crypto.randomUUID().slice(0, 8)}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${failing.port}/x`,
            method: "GET",
            responses: {
              200: { type: "object" },
              404: {
                type: "object",
                properties: { pan: { type: "string", secret: true }, why: { type: "string" } },
              },
            },
          },
          timeout: 2000,
          on_error: [{ code: ["http.404"], goto: "$carry" }],
          switch: [{ goto: "end" }],
        },
        { id: "carry", output: { seen: "$: error.data" }, switch: [{ goto: "end" }] },
      ],
    } as never,
  });

  const { data: started } = await client.POST("/instances", { body: { process: name } });
  expect(await waitForInstance(started!.id)).toBe("completed");
  const { data } = await client.GET("/instances/{id}", { params: { path: { id: started!.id } } });
  expect(((data?.context?.outputs as any)?.carry?.seen)?.pan).toBe("***");
  expect((data?.context?.error as any)?.data?.pan, "the slot the declaration types").toBe("***");
  failing.stop();
});

// The boundary this draws, stated rather than discovered later: the declaration is the
// CALLER's, so it governs the caller's view. On the child's own row the payload is whatever
// the child chose to attach, and the child's definition says nothing about it being secret —
// the same way a caller's result_schema never governs the child's own output row.
test("the declaration governs the caller's view, not the child's own row", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `sec_bound_child_${uid}`;
  await client.PUT("/definitions", {
    body: {
      name: child,
      tasks: [
        {
          id: "charge",
          switch: [
            { case: "true", raise: { code: "card_declined", message: "no", data: { pan: PAN } } },
            { goto: "end" },
          ],
        },
      ],
    },
  });

  const parent = `sec_bound_parent_${uid}`;
  await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "pay",
          action: {
            type: "child" as const,
            name: child,
            raises: { card_declined: { type: "object", properties: { pan: { type: "string", secret: true } } } },
          },
          on_error: [{ code: ["card_declined"], goto: "$carry" }],
          switch: [{ goto: "end" }],
        },
        { id: "carry", output: { seen: "$: error.data" }, switch: [{ goto: "end" }] },
      ],
    } as never,
  });

  const { data: started } = await client.POST("/instances", { body: { process: parent } });
  expect(await waitForInstance(started!.id)).toBe("completed");
  const { data } = await client.GET("/instances/{id}", { params: { path: { id: started!.id } } });
  expect(((data?.context?.outputs as any)?.carry?.seen)?.pan).toBe("***");
  // Directly in the slot, not only where a projection carried it into an output: the error
  // slot is the one the context schema had no place for at all.
  expect((data?.context?.error as any)?.data?.pan).toBe("***");

  const childId = (data?.context as any)?._children?.pay as string;
  const { data: kid } = await client.GET("/instances/{id}", { params: { path: { id: childId } } });
  expect(
    (kid?.context?.error as any)?.data?.pan,
    "the child declared nothing about this value, so its own row shows what it attached",
  ).toBe(PAN);
});

// Log scrubbing works by VALUE, and the values are collected from the context by schema — so
// a payload that no output projects is only knowable through the declaration on error.data
// itself. Nothing here exports anything: the interpolated message is the only route out.
test("a payload nothing projects is still scrubbed from the audit trail", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `probe_log_child_${uid}`;
  await client.PUT("/definitions", {
    body: {
      name: child,
      tasks: [
        {
          id: "charge",
          switch: [
            { case: "true", raise: { code: "card_declined", message: "no", data: { pan: PAN, why: "51" } } },
            { goto: "end" },
          ],
        },
      ],
    },
  });

  const parent = `probe_log_parent_${uid}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "pay",
          action: {
            type: "child" as const,
            name: child,
            raises: {
              card_declined: {
                type: "object",
                properties: { pan: { type: "string", secret: true }, why: { type: "string" } },
                required: ["pan", "why"],
              },
            },
          },
          on_error: [{ code: ["card_declined"], goto: "$carry" }],
          switch: [{ goto: "end" }],
        },
        {
          id: "carry",
          switch: [
            { case: "true", raise: { code: "gave_up", message: "declined ${error.data.pan}" } },
            { goto: "end" },
          ],
        },
      ],
    } as never,
  });
  expect(putErr).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: parent } });
  expect(await waitForInstance(started!.id)).toBe("raised");
  const { data: logs } = await client.GET("/instances/{id}/logs", { params: { path: { id: started!.id } } });
  expect(JSON.stringify(logs), "the rendered message put it in the trail").not.toContain(PAN);
});
