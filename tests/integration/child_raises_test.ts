import { expect, test } from "vitest";
import { client, waitForInstance, objectAt, spliceObjects, childrenOfTask } from "../helpers/client.ts";

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

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  expect(data?.state?.output).toEqual({ wait: 3600, why: "51" });
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

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  expect(
    data?.state?.output,
    "the code is replaced, so the rule naming the raised code no longer fires",
  ).toEqual({ via: "mismatch", code: "output.invalid" });

  // The error being diagnosed survives: the child is still raised, with its own code.
  const childId = (await childrenOfTask(started!.id, "pay")) as string;
  const { data: kid } = await client.GET("/instances/{id}/detail", { params: { path: { id: childId } } });
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

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
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
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  expect(data?.state?.output).toEqual({ wait: 3600 });

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

// The slot refuses three things it cannot mean. Each is a decision with a message that has
// to point somewhere: null at "omit or {}", a wrong action type at the child family, a dotted
// key at the fact that no engine code carries a declared payload.
test("raises refuses a boolean, a non-declaring action, and a code that is not one", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `raises_refuse_child_${uid}`;
  await putDecliner(child, { decline_code: "51", retry_after: 3600 });

  async function refused(suffix: string, action: unknown, expected: string) {
    const { error } = await client.PUT("/definitions", {
      body: {
        name: `raises_refuse_${suffix}_${uid}`,
        tasks: [{ id: "pay", action: action as never, switch: [{ goto: "end" }] }],
      },
    });
    expect(JSON.stringify(error), `${suffix} must be refused with its own message`).toContain(expected);
  }

  // null used to be refused ("omitting the code already says that"). It is now the third
  // declaration state — the code is declared and carries nothing — because on an external
  // task omitting means NOT SUBMITTABLE, so the two stopped being the same statement.
  // A boolean takes over the refusal: raises[code] is a schema position, and genroc has no
  // boolean schemas.
  await refused(
    "boolean",
    { type: "child", name: child, raises: { card_declined: true } },
    "boolean schemas are not supported",
  );
  await refused(
    "fetch",
    { type: "fetch", url: "http://localhost:1/x", raises: { card_declined: {} } },
    "only valid on a child",
  );
  await refused(
    "dotted",
    { type: "child", name: child, raises: { "output.invalid": {} } },
    "is not a raise code",
  );
});

// The third declaration state: {} is the top type — present, forwardable whole, and not
// readable field by field until something restates its shape.
test("{} exposes the payload opaquely: forwardable, but a field read is refused", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `raises_open_child_${uid}`;
  await putDecliner(child, { decline_code: "51", retry_after: 3600 });

  const def = (name: string, output: Record<string, string>) => ({
    name,
    tasks: [
      {
        id: "pay",
        action: { type: "child" as const, name: child, raises: { card_declined: {} } },
        on_error: [{ code: ["card_declined"], goto: "$carry" }],
        switch: [{ goto: "end" }],
      },
      { id: "carry", output, switch: [{ goto: "end" }] },
    ],
    output: "$: outputs.carry",
  });

  const { error: fieldErr } = await client.PUT("/definitions", {
    body: def(`raises_open_field_${uid}`, { why: "$: error.data.decline_code" }),
  });
  expect(JSON.stringify(fieldErr), "the top type is carried, never read").toBeTruthy();
  expect(fieldErr).toBeDefined();

  const whole = `raises_open_whole_${uid}`;
  const { error: wholeErr } = await client.PUT("/definitions", {
    body: def(whole, { payload: "$: error.data" }),
  });
  expect(wholeErr).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: whole } });
  expect(await waitForInstance(started!.id)).toBe("completed");
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  expect((data?.state?.output as any)?.payload).toEqual({ decline_code: "51", retry_after: 3600 });
});

// The conform NORMALIZES, exactly as result_schema does on the success path: the caller sees
// the shape it declared, not whatever the child happened to attach.
test("the payload is conformed, not passed through: extras dropped, defaults filled", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `raises_norm_child_${uid}`;
  await putDecliner(child, { decline_code: "51", retry_after: 3600, note: "chatty" });

  const name = `raises_norm_${uid}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "pay",
          action: {
            type: "child" as const,
            name: child,
            // `note` is undeclared and must be dropped; `channel` is declared with a default
            // the child never sent, and must appear.
            raises: {
              card_declined: {
                type: "object",
                properties: {
                  decline_code: { type: "string" },
                  channel: { type: "string", default: "unknown" },
                },
              },
            },
          },
          on_error: [{ code: ["card_declined"], goto: "$carry" }],
          switch: [{ goto: "end" }],
        },
        { id: "carry", output: { seen: "$: error.data" }, switch: [{ goto: "end" }] },
      ],
      output: "$: outputs.carry",
    },
  });
  expect(putErr).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: name } });
  expect(await waitForInstance(started!.id)).toBe("completed");
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  expect((data?.state?.output as any)?.seen).toEqual({ decline_code: "51", channel: "unknown" });
});

// child_list declares on the action (one process for every element), and the first raised
// slot in index order is the one whose payload crosses.
test("child_list declares on the action, and the first raised slot's payload crosses", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `raises_list_child_${uid}`;
  await putDecliner(child, { decline_code: "51", retry_after: 3600 });

  const name = `raises_list_${uid}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "pay",
          action: {
            type: "child_list" as const,
            name: child,
            over: "$: [1, 2]",
            raises: { card_declined: DECLINE_SHAPE },
          },
          on_error: [{ code: ["card_declined"], goto: "$backoff" }],
          switch: [{ goto: "end" }],
        },
        {
          id: "backoff",
          output: { slot: "$: error.child_index", why: "$: error.data.decline_code" },
          switch: [{ goto: "end" }],
        },
      ],
      output: "$: outputs.backoff",
    },
  });
  expect(putErr).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: name } });
  expect(await waitForInstance(started!.id)).toBe("completed");
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  expect(data?.state?.output).toEqual({ slot: 0, why: "51" });
});

// A declaration is an ordinary schema document, so it may name a shared definition — which
// means the process pool has to be baked into it before inference embeds it in a context.
test("a raises schema may be a $ref into the process $defs", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `raises_ref_child_${uid}`;
  await putDecliner(child, { decline_code: "51", retry_after: 3600 });

  const name = `raises_ref_${uid}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      $defs: { decline: DECLINE_SHAPE },
      tasks: [
        {
          id: "pay",
          action: {
            type: "child" as const,
            name: child,
            raises: { card_declined: { $ref: "#/$defs/decline" } },
          },
          on_error: [{ code: ["card_declined"], goto: "$backoff" }],
          switch: [{ goto: "end" }],
        },
        { id: "backoff", output: { wait: "$: error.data.retry_after" }, switch: [{ goto: "end" }] },
      ],
      output: "$: outputs.backoff",
    },
  });
  expect(putErr).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: name } });
  expect(await waitForInstance(started!.id)).toBe("completed");
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  expect(data?.state?.output).toEqual({ wait: 3600 });
});

// Past the 2 KiB inline cutoff the payload lives in the object store, so crossing to the
// parent means resolving it before the conform — the path a stack trace actually takes.
test("a payload past the inline cutoff externalizes and still crosses whole", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `raises_big_child_${uid}`;
  const blob = "S".repeat(8 * 1024);
  await putDecliner(child, { decline_code: "51", retry_after: 3600, trace: blob });

  const name = `raises_big_${uid}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "pay",
          action: {
            type: "child" as const,
            name: child,
            raises: {
              card_declined: { type: "object", properties: { trace: { type: "string" } }, required: ["trace"] },
            },
          },
          on_error: [{ code: ["card_declined"], goto: "$keep" }],
          switch: [{ goto: "end" }],
        },
        { id: "keep", output: { trace: "$: error.data.trace" }, switch: [{ goto: "end" }] },
      ],
      output: "$: outputs.keep",
    },
  });

  const { data: started } = await client.POST("/instances", { body: { process: name } });
  expect(await waitForInstance(started!.id)).toBe("completed");

  // The proof that the object store was involved is on the CHILD's row, where the payload was
  // written: the fault slot is enveloped alone, so past the cutoff it reads as a {ref, size}
  // marker until something asks for it.
  const { data: lazy } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  const childId = (await childrenOfTask(started!.id, "pay")) as string;
  const { data: kid } = await client.GET("/instances/{id}/detail", { params: { path: { id: childId } } });
  // The cut takes the big leaf inside the raised payload, so the listing names a path THROUGH
  // the state slot it was cut from.
  expect(
    (kid!.objects ?? []).some((o: any) => o.path[0] === "state" && o.path[1] === "_error_data"),
    "8 KiB is past the 2 KiB cutoff, so the payload must be externalized",
  ).toBe(true);

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  await spliceObjects(data);
  expect((data?.state?.output as any)?.trace).toBe(blob);
});

// A wildcard reaches codes no key declares, so it admits null even where every code it
// happens to match is declared — the raise set belongs to another definition.
test("a wildcard rule widens the type to admit null; the literal does not", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `raises_wild_child_${uid}`;
  await putDecliner(child, { decline_code: "51", retry_after: 3600 });

  const def = (name: string, code: string) => ({
    name,
    tasks: [
      {
        id: "pay",
        action: { type: "child" as const, name: child, raises: { card_declined: DECLINE_SHAPE } },
        on_error: [{ code: [code], goto: "$explain" }],
        switch: [{ goto: "end" }],
      },
      {
        id: "explain",
        switch: [
          { case: "true", raise: { code: "gave_up", message: "declined: ${error.data.decline_code}" } },
          { goto: "end" },
        ],
      },
    ],
  });

  const { error: wildErr } = await client.PUT("/definitions", { body: def(`raises_wild_${uid}`, "card_%") });
  expect(JSON.stringify(wildErr), "a wildcard can reach a code nothing declares").toContain("may be null");

  const { error: litErr } = await client.PUT("/definitions", { body: def(`raises_lit_${uid}`, "card_declined") });
  expect(litErr, "the declared literal is exactly covered").toBeUndefined();
});

// checkDeclaredRaises is the error-channel member of the child/parent compatibility family
// (input subset, output narrowing, R5 reachability), so it has to hold on every shape that
// family covers — and on the one code kind that is never raisable.
test("a declaration is checked against the raise set on every child shape", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `raises_shape_child_${uid}`;
  await putDecliner(child, { decline_code: "51", retry_after: 3600 });

  const cases: [string, Record<string, unknown>][] = [
    ["map", { type: "child_map", children: { a: { name: child, raises: { card_expired: DECLINE_SHAPE } } } }],
    ["list", { type: "child_list", name: child, over: "$: [1]", raises: { card_expired: DECLINE_SHAPE } }],
  ];
  for (const [label, action] of cases) {
    const { error } = await client.PUT("/definitions", {
      body: {
        name: `raises_shape_${label}_${uid}`,
        tasks: [{ id: "pay", action: action as never, switch: [{ goto: "end" }] }],
      },
    });
    expect(JSON.stringify(error), `${label} must check its own declarations`).toContain("never raises");
  }
});

// A panic code is excluded from raises(D) by construction, so no declaration can ever apply
// to one: nothing catches a panic, and its payload reaches an operator only.
test("a panic-only code cannot be declared — nothing can ever catch it", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `raises_panic_child_${uid}`;
  await client.PUT("/definitions", {
    body: {
      name: child,
      tasks: [
        {
          id: "check",
          switch: [
            { case: "true", panic: { code: "script_broken", message: "broken", data: { kind: "syntax" } } },
            { goto: "end" },
          ],
        },
      ],
    },
  });

  const { error } = await client.PUT("/definitions", {
    body: {
      name: `raises_panic_parent_${uid}`,
      tasks: [
        {
          id: "run",
          action: { type: "child" as const, name: child, raises: { script_broken: { type: "object" } } },
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(JSON.stringify(error), "a panic code is not in the raise set").toContain("never raises");
});

// A self-reference resolves to the definition being registered, so its own raise set is what
// a declaration on it is checked against — the raises analogue of R5 terminating on itself.
test("a self-referencing call checks declarations against the definition being registered", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const name = `raises_self_${uid}`;
  const def = (code: string) => ({
    name,
    input_schema: { type: "object", properties: { depth: { type: "integer" } } },
    tasks: [
      // The guard is the entry task: it raises at depth, and everything else routes from it.
      {
        id: "guard",
        switch: [
          {
            case: "(input.depth ?? 0) > 3",
            raise: { code: "too_deep", message: "too deep", data: { decline_code: "d", retry_after: 1 } },
          },
          { goto: "next" },
        ],
      },
      {
        id: "recurse",
        action: {
          type: "child" as const,
          name,
          input: { depth: "$: (input.depth ?? 0) + 1" },
          raises: { [code]: DECLINE_SHAPE },
        },
        on_error: [{ code: ["too_deep"], goto: "end" }],
        switch: [{ goto: "end" }],
      },
    ],
  });

  const { error: ok } = await client.PUT("/definitions", { body: def("too_deep") });
  expect(ok, "the code this definition raises is declarable on a call to itself").toBeUndefined();

  const { error: typo } = await client.PUT("/definitions", { body: def("too_shallow") });
  expect(JSON.stringify(typo)).toContain("never raises");
});

// A wildcard catching SEVERAL declared codes combines their shapes: error.data is the union
// of every declaration the rule can reach, and the arm that arrives is the one the raised
// code declared. A field only one arm carries is therefore nullable, not refused.
const DECLINED_SHAPE = {
  type: "object",
  properties: { kind: { type: "string" }, decline_code: { type: "string" } },
  required: ["kind", "decline_code"],
} as const;
const EXPIRED_SHAPE = {
  type: "object",
  properties: { kind: { type: "string" }, expired_on: { type: "string" } },
  required: ["kind", "expired_on"],
} as const;

// A child that picks its code from its input, so one definition produces both arms.
async function putTwoCoded(name: string) {
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      input_schema: { type: "object", properties: { expired: { type: "boolean" } } },
      tasks: [
        {
          id: "charge",
          switch: [
            {
              case: "input.expired == true",
              raise: { code: "card_expired", message: "expired", data: { kind: "expired", expired_on: "2026-01" } },
            },
            {
              case: "true",
              raise: { code: "card_declined", message: "declined", data: { kind: "declined", decline_code: "51" } },
            },
            { goto: "end" },
          ],
        },
      ],
    } as never,
  });
  expect(error).toBeUndefined();
}

test("a % rule unions every declared shape it can reach, and the raised code decides the arm", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `union_child_${uid}`;
  const parent = `union_parent_${uid}`;
  await putTwoCoded(child);

  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name: parent,
      input_schema: { type: "object", properties: { expired: { type: "boolean" } } },
      tasks: [
        {
          id: "pay",
          action: {
            type: "child" as const,
            name: child,
            input: { expired: "$: input.expired ?? false" },
            raises: { card_declined: DECLINED_SHAPE, card_expired: EXPIRED_SHAPE },
          },
          on_error: [{ code: ["card_%"], goto: "$handle" }],
          switch: [{ goto: "end" }],
        },
        {
          id: "handle",
          // Both arms' own fields are read here: neither reads unless BOTH declarations are
          // in the union, which is what makes this a test of combining rather than of one arm.
          output: {
            seen: "$: error.data",
            kind: "$: error.data.kind",
            declined: "$: error.data.decline_code",
            expired: "$: error.data.expired_on",
          },
          switch: [{ goto: "end" }],
        },
      ],
      output: "$: outputs.handle",
    } as never,
  });
  expect(putErr).toBeUndefined();

  for (const [expired, expectedArm] of [
    [false, { kind: "declined", decline_code: "51" }],
    [true, { kind: "expired", expired_on: "2026-01" }],
  ] as const) {
    const { data: started } = await client.POST("/instances", {
      body: { process: parent, input: { expired } },
    });
    expect(await waitForInstance(started!.id)).toBe("completed");
    const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
    const out = data?.state?.output as any;
    expect(out?.seen, `expired=${expired} must arrive as its own declared shape`).toEqual(expectedArm);
    expect(out?.kind).toBe(expectedArm.kind);
    // The other arm's field is present in the TYPE and null in this VALUE.
    expect(expired ? out?.declined : out?.expired).toBeNull();
  }
});

test("a field only one arm of the union declares reads as null when the other arrives", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `union_one_child_${uid}`;
  const parent = `union_one_parent_${uid}`;
  await putTwoCoded(child);

  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name: parent,
      input_schema: { type: "object", properties: { expired: { type: "boolean" } } },
      tasks: [
        {
          id: "pay",
          action: {
            type: "child" as const,
            name: child,
            input: { expired: "$: input.expired ?? false" },
            raises: { card_declined: DECLINED_SHAPE, card_expired: EXPIRED_SHAPE },
          },
          on_error: [{ code: ["card_%"], goto: "$handle" }],
          switch: [{ goto: "end" }],
        },
        // decline_code exists in one arm only. The read is legal — the union admits it — and
        // it is null on the run where the other arm arrived.
        { id: "handle", output: { code: "$: error.data.decline_code" }, switch: [{ goto: "end" }] },
      ],
      output: "$: outputs.handle",
    } as never,
  });
  expect(putErr, "a one-arm field is nullable, not unreadable").toBeUndefined();

  const { data: started } = await client.POST("/instances", {
    body: { process: parent, input: { expired: true } },
  });
  expect(await waitForInstance(started!.id)).toBe("completed");
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  expect((data?.state?.output as any)?.code, "card_expired carries no decline_code").toBeNull();
});

// M2's typing claim: a rule's `case` is checked against the payload of the codes THAT RULE
// names, not the union a routed task would see. Two codes with incompatible shapes make the
// difference observable — the same expression is legal under one rule and rejected under the
// other. specs/child-error-handling.md M2.
const NAMED = { type: "object", properties: { name: { type: "string" } }, required: ["name"] } as const;
const NUMBERED = { type: "object", properties: { digits: { type: "integer" } }, required: ["digits"] } as const;

async function putCaseScopeChild(name: string) {
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      // Both codes from ONE task: raises(D) is a syntactic scan, so a case that never fires
      // still puts its code in the set — and a second task nothing routes to would be
      // rejected as unreachable.
      tasks: [
        {
          id: "go",
          switch: [
            { case: "false", raise: { code: "numbered", message: "d", data: { digits: 1 } } as never },
            { raise: { code: "named", message: "n", data: { name: "X" } } as never },
          ],
        },
      ],
    },
  });
  expect(error).toBeUndefined();
}

async function callerWithCase(child: string, code: string, caseExpr: string) {
  return client.PUT("/definitions", {
    body: {
      name: `case_scope_${crypto.randomUUID().slice(0, 8)}`,
      tasks: [
        {
          id: "pay",
          action: { type: "child" as const, name: child, raises: { named: NAMED, numbered: NUMBERED } },
          on_error: [{ code: [code], case: caseExpr, goto: "$handled" } as never],
          switch: [{ goto: "end" }],
        },
        { id: "handled", output: { ok: true }, switch: [{ goto: "end" }] },
      ],
    } as never,
  });
}

test("an on_error case is typed by the codes its own rule names", async () => {
  const child = `case_scope_child_${crypto.randomUUID().slice(0, 8)}`;
  await putCaseScopeChild(child);

  // `named` declares `name`, so the rule that catches it may read it — and as a NON-NULL
  // string, which a union across both codes could not offer.
  const ok = await callerWithCase(child, "named", 'error.data.name == "X"');
  expect(ok.error, "a case may read what its own code declares").toBeUndefined();

  // `numbered` does not. Were the case typed against the task's whole catchable set — both
  // codes — `name` would be present-but-optional here and this would be accepted.
  const bad = await callerWithCase(child, "numbered", 'error.data.name == "X"');
  expect(bad.error, "a case must not read a field only ANOTHER code declares").toBeDefined();
  expect(JSON.stringify(bad.error)).toContain("name");
});
