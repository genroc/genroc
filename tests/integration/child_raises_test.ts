import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// `raises` on a child call declares what a raised fault's payload looks like, keyed by raise
// code — the error channel's counterpart to result_schema, and declared by the CALLER so a
// generic child stays generic. Declared → readable as error.data; undeclared → absent;
// mismatched → output.invalid, replacing the raised code. specs/error-extensions.md §X2-c.

const DECLINE_SHAPE = {
  type: "object",
  properties: { decline_code: { type: "string" }, retry_after: { type: "integer" } },
  required: ["decline_code", "retry_after"],
} as const;

// A child that raises `card_declined`, optionally attaching a payload.
async function putDecliner(name: string, data?: unknown) {
  const raise: Record<string, unknown> = { code: "card_declined", message: "the card was declined" };
  if (data !== undefined) raise.data = data;
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        { id: "charge", switch: [{ case: "true", raise: raise as never }, { goto: "end" }] },
      ],
    },
  });
  expect(error).toBeUndefined();
}

test("a declared code makes the payload readable as error.data at the routed task", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `raises_child_${uid}`;
  const parent = `raises_parent_${uid}`;
  await putDecliner(child, { decline_code: "51", retry_after: 3600 });

  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "pay",
          action: {
            type: "child" as const,
            name: child,
            raises: { card_declined: DECLINE_SHAPE },
          },
          on_error: [{ code: ["card_declined"], goto: "$backoff" }],
          switch: [{ goto: "end" }],
        },
        {
          // The rule catches one declared code, so error.data is that shape and non-null —
          // a bare member access must type-check without a narrowing or a `??`.
          id: "backoff",
          output: { wait: "$: error.data.retry_after", why: "$: error.data.decline_code" },
          switch: [{ goto: "end" }],
        },
      ],
      output: "$: outputs.backoff",
    },
  });
  expect(putErr).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: parent } });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  expect(data?.context?.output).toEqual({ wait: 3600, why: "51" });
});

test("an undeclared code leaves error.data absent — the read is a registration error", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `raises_undecl_child_${uid}`;
  const parent = `raises_undecl_parent_${uid}`;
  await putDecliner(child, { decline_code: "51", retry_after: 3600 });

  const { error } = await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "pay",
          // No raises: the child still attaches a payload, and it still must not be reachable.
          action: { type: "child" as const, name: child },
          on_error: [{ code: ["card_declined"], goto: "$backoff" }],
          switch: [{ goto: "end" }],
        },
        { id: "backoff", output: { wait: "$: error.data.retry_after" }, switch: [{ goto: "end" }] },
      ],
    },
  });
  expect(
    error,
    "undeclared data is never accessible — reading it must fail where every other missing slot does",
  ).toBeDefined();
});

test("a payload that does not fit the declaration replaces the raised code with output.invalid", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `raises_bad_child_${uid}`;
  const parent = `raises_bad_parent_${uid}`;
  // A string where the caller declared an object: both definitions are self-consistent, and
  // only the caller's bet about the shape is wrong.
  await putDecliner(child, "the card was declined");

  await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "pay",
          action: { type: "child" as const, name: child, raises: { card_declined: DECLINE_SHAPE } },
          on_error: [
            { code: ["card_declined"], goto: "$by_code" },
            { code: ["output.invalid"], goto: "$by_mismatch" },
          ],
          switch: [{ goto: "end" }],
        },
        { id: "by_code", output: { via: "code" }, switch: [{ goto: "end" }] },
        { id: "by_mismatch", output: { via: "mismatch", code: "$: error.code" }, switch: [{ goto: "end" }] },
      ],
      output: "$: outputs.by_mismatch ?? outputs.by_code",
    },
  });

  const { data: started } = await client.POST("/instances", { body: { process: parent } });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  expect(
    data?.context?.output,
    "the code is replaced, so the rule naming the raised code no longer fires",
  ).toEqual({ via: "mismatch", code: "output.invalid" });

  // The error being diagnosed survives: the child is still raised, with its own code.
  const childId = (data?.context as any)?._children?.pay as string;
  const { data: kid } = await client.GET("/instances/{id}", { params: { path: { id: childId } } });
  expect(kid?.status).toBe("raised");
  expect(kid?.error_code).toBe("card_declined");
});

test("a declared code the child raises without data is the same lost bet", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `raises_nodata_child_${uid}`;
  const parent = `raises_nodata_parent_${uid}`;
  await putDecliner(child); // raises card_declined, attaches nothing

  await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "pay",
          action: { type: "child" as const, name: child, raises: { card_declined: DECLINE_SHAPE } },
          on_error: [{ code: ["card_declined"], goto: "$backoff" }],
          switch: [{ goto: "end" }],
        },
        { id: "backoff", output: { wait: "$: error.data.retry_after" }, switch: [{ goto: "end" }] },
      ],
    },
  });

  const { data: started } = await client.POST("/instances", { body: { process: parent } });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("failed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  expect(
    data?.error_code,
    "a declaration is a bet on a shape; nothing is not that shape",
  ).toBe("output.invalid");
});

test("a child_map declares per entry, and the action-level slot is refused", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `raises_map_child_${uid}`;
  await putDecliner(child, { decline_code: "51", retry_after: 3600 });

  const perEntry = `raises_map_ok_${uid}`;
  const { error: okErr } = await client.PUT("/definitions", {
    body: {
      name: perEntry,
      tasks: [
        {
          id: "pay",
          action: {
            type: "child_map" as const,
            children: { a: { name: child, raises: { card_declined: DECLINE_SHAPE } } },
          },
          on_error: [{ code: ["card_declined"], goto: "$backoff" }],
          switch: [{ goto: "end" }],
        },
        { id: "backoff", output: { wait: "$: error.data.retry_after" }, switch: [{ goto: "end" }] },
      ],
      output: "$: outputs.backoff",
    },
  });
  expect(okErr).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: perEntry } });
  expect(await waitForInstance(started!.id)).toBe("completed");
  const { data } = await client.GET("/instances/{id}", { params: { path: { id: started!.id } } });
  expect(data?.context?.output).toEqual({ wait: 3600 });

  const wrong = `raises_map_bad_${uid}`;
  const { error } = await client.PUT("/definitions", {
    body: {
      name: wrong,
      tasks: [
        {
          id: "pay",
          action: {
            type: "child_map" as const,
            children: { a: { name: child } },
            raises: { card_declined: DECLINE_SHAPE },
          } as never,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(JSON.stringify(error), "entries can be different processes, so the slot is theirs").toContain(
    "children",
  );
});

test("declaring a code the child never raises is refused, like a rule for one", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `raises_typo_child_${uid}`;
  await putDecliner(child, { decline_code: "51", retry_after: 3600 });

  const { error } = await client.PUT("/definitions", {
    body: {
      name: `raises_typo_parent_${uid}`,
      tasks: [
        {
          id: "pay",
          action: { type: "child" as const, name: child, raises: { card_expired: DECLINE_SHAPE } },
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(JSON.stringify(error)).toContain("never raises");
});

// A child_map's entries are different processes, so a declaration on one says nothing about
// the entry that actually raised. Where the cover has a gap the payload can be absent at
// runtime, and the type has to say so — otherwise a handler reads a slot that is not there.
test("a code only some child_map entries declare is nullable; declared by all, it is not", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const a = `raises_cover_a_${uid}`;
  const b = `raises_cover_b_${uid}`;
  await putDecliner(a, { decline_code: "51", retry_after: 3600 });
  await putDecliner(b, { decline_code: "61", retry_after: 60 });

  const def = (name: string, bDeclares: boolean) => ({
    name,
    tasks: [
      {
        id: "pay",
        action: {
          type: "child_map" as const,
          children: {
            a: { name: a, raises: { card_declined: DECLINE_SHAPE } },
            b: bDeclares ? { name: b, raises: { card_declined: DECLINE_SHAPE } } : { name: b },
          },
        },
        on_error: [{ code: ["card_declined"], goto: "$backoff" }],
        switch: [{ goto: "end" }],
      },
      {
        // An interpolation is the slot that refuses a null, which is where the gap surfaces.
        id: "backoff",
        switch: [
          {
            case: "true",
            raise: { code: "gave_up", message: "retry after ${error.data.retry_after}s" },
          },
          { goto: "end" },
        ],
      },
    ],
  });

  const { error: gapErr } = await client.PUT("/definitions", { body: def(`raises_gap_${uid}`, false) });
  expect(
    JSON.stringify(gapErr),
    "entry b declares nothing, so a card_declined from b arrives with no data at all",
  ).toContain("may be null");

  const { error: fullErr } = await client.PUT("/definitions", { body: def(`raises_full_${uid}`, true) });
  expect(fullErr, "with every entry covered the same read is sound").toBeUndefined();
});
