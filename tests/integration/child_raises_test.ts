import { expect, test } from "vitest";
import { client, waitForInstance, objectAt, spliceObjects, childrenOfTask } from "../helpers/client.ts";

// `raises` types a raised payload per code, declared by the CALLER: declared → error.data,
// undeclared → absent, mismatched → result.invalid. specs/error-extensions.md §X2-c.

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

// An OPAQUE (`{}`) payload is the only one registration cannot judge, so the runtime conform is
// tested through this child.
async function putOpaqueDecliner(name: string) {
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      input_schema: { type: "object", properties: { payload: {} } },
      tasks: [
        {
          id: "charge",
          switch: [
            { case: "true", raise: { code: "card_declined", message: "the card was declined", data: "$: input.payload" } },
            { goto: "end" },
          ],
        },
      ],
    } as never,
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
          output: { wait: "$: last_error.data.retry_after", why: "$: last_error.data.decline_code" },
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
  expect(data?.output).toEqual({ wait: 3600, why: "51" });
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
        { id: "backoff", output: { wait: "$: last_error.data.retry_after" }, switch: [{ goto: "end" }] },
      ],
    },
  });
  expect(
    error,
    "undeclared data is never accessible — reading it must fail where every other missing slot does",
  ).toBeDefined();
});

test("a payload that does not fit the declaration replaces the raised code with result.invalid", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `raises_bad_child_${uid}`;
  const parent = `raises_bad_parent_${uid}`;
  // The child's payload is opaque, so only the runtime conform can catch this string.
  await putOpaqueDecliner(child);

  await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "pay",
          action: {
            type: "child" as const,
            name: child,
            input: { payload: "the card was declined" },
            raises: { card_declined: DECLINE_SHAPE },
          },
          on_error: [
            { code: ["card_declined"], goto: "$by_code" },
            { code: ["result.invalid"], goto: "$by_mismatch" },
          ],
          switch: [{ goto: "end" }],
        },
        { id: "by_code", output: { via: "code" }, switch: [{ goto: "end" }] },
        { id: "by_mismatch", output: { via: "mismatch", code: "$: last_error.code" }, switch: [{ goto: "end" }] },
      ],
      output: "$: outputs.by_mismatch ?? outputs.by_code",
    },
  });

  const { data: started } = await client.POST("/instances", { body: { process: parent } });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  expect(
    data?.output,
    "the code is replaced, so the rule naming the raised code no longer fires",
  ).toEqual({ via: "mismatch", code: "result.invalid" });

  // The error being diagnosed survives: the child is still raised, with its own code.
  const childId = (await childrenOfTask(started!.id, "pay")) as string;
  const { data: kid } = await client.GET("/instances/{id}/detail", { params: { path: { id: childId } } });
  expect(kid?.status).toBe("raised");
  expect(kid?.error_code).toBe("card_declined");
});

// Checked at registration via NarrowsTo, like result_schema; only an untypable `{}` payload
// reaches the conform at collect. specs/error-extensions.md §X2-c.
test("a payload the child can never produce is refused where it is declared", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const wrongType = `raises_static_type_${uid}`;
  const noData = `raises_static_nodata_${uid}`;
  await putDecliner(wrongType, "the card was declined"); // a string where an object is declared
  await putDecliner(noData); // raises card_declined, attaches nothing

  const parent = (name: string, child: string) => ({
    name,
    tasks: [
      {
        id: "pay",
        action: { type: "child" as const, name: child, raises: { card_declined: DECLINE_SHAPE } },
        on_error: [{ code: ["card_declined"], goto: "$backoff" }],
        switch: [{ goto: "end" }],
      },
      { id: "backoff", output: { wait: "$: last_error.data.retry_after" }, switch: [{ goto: "end" }] },
    ],
  });

  const { error: typeErr } = await client.PUT("/definitions", {
    body: parent(`raises_static_type_parent_${uid}`, wrongType),
  });
  expect(
    JSON.stringify(typeErr),
    "a string is not an object on any run, so the declaration is refused rather than lost later",
  ).toContain("string is not accepted where object is expected");

  const { error: nodataErr } = await client.PUT("/definitions", {
    body: parent(`raises_static_nodata_parent_${uid}`, noData),
  });
  expect(
    JSON.stringify(nodataErr),
    "a clause attaching nothing clears the slot; a declaration is a bet on a shape, and nothing is not that shape",
  ).toContain("null is not accepted where object is expected");
});

test("a code raised from two clauses is checked against both", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const child = `raises_union_child_${uid}`;
  await client.PUT("/definitions", {
    body: {
      name: child,
      input_schema: { type: "object", properties: { early: { type: "boolean" } } },
      tasks: [
        {
          id: "charge",
          switch: [
            {
              case: "input.early ?? false",
              raise: { code: "card_declined", message: "no", data: { decline_code: "51", retry_after: 3600 } },
            },
            { goto: "$settle" },
          ],
        },
        {
          id: "settle",
          switch: [
            { case: "true", raise: { code: "card_declined", message: "no", data: { decline_code: "61" } } },
            { goto: "end" },
          ],
        },
      ],
    } as never,
  });

  const call = (name: string, shape: unknown) =>
    client.PUT("/definitions", {
      body: {
        name,
        tasks: [
          {
            id: "pay",
            action: { type: "child" as const, name: child, raises: { card_declined: shape } },
            switch: [{ goto: "end" }],
          },
        ],
      } as never,
    });

  const { error: bothErr } = await call(`raises_union_both_${uid}`, DECLINE_SHAPE);
  expect(
    JSON.stringify(bothErr),
    "the second clause never sets retry_after, so requiring it can fail on a run the first clause did not take",
  ).toContain("retry_after");

  const { error: coveredErr } = await call(`raises_union_ok_${uid}`, {
    type: "object",
    properties: { decline_code: { type: "string" }, retry_after: { type: "integer" } },
    required: ["decline_code"],
  });
  expect(coveredErr, "what both clauses guarantee is what a declaration may require").toBeUndefined();
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
        { id: "backoff", output: { wait: "$: last_error.data.retry_after" }, switch: [{ goto: "end" }] },
      ],
      output: "$: outputs.backoff",
    },
  });
  expect(okErr).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: perEntry } });
  expect(await waitForInstance(started!.id)).toBe("completed");
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  expect(data?.output).toEqual({ wait: 3600 });

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

// Entries are different processes, so a declaration on one says nothing about the one that raised.
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
            raise: { code: "gave_up", message: "retry after ${last_error.data.retry_after}s" },
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

  // null is a valid declaration (declared, carries nothing), so a boolean carries the refusal:
  // raises[code] is a schema position, and genroc has no boolean schemas.
  await refused(
    "boolean",
    { type: "child", name: child, raises: { card_declined: true } },
    "boolean schemas are not supported",
  );
  await refused(
    "fetch",
    { type: "fetch", method: "post", url: "http://localhost:1/x", raises: { card_declined: {} } },
    "only valid on a child",
  );
  await refused(
    "dotted",
    { type: "child", name: child, raises: { "result.invalid": {} } },
    "is not a raise code",
  );
});

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
    body: def(`raises_open_field_${uid}`, { why: "$: last_error.data.decline_code" }),
  });
  expect(JSON.stringify(fieldErr), "the top type is carried, never read").toBeTruthy();
  expect(fieldErr).toBeDefined();

  const whole = `raises_open_whole_${uid}`;
  const { error: wholeErr } = await client.PUT("/definitions", {
    body: def(whole, { payload: "$: last_error.data" }),
  });
  expect(wholeErr).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: whole } });
  expect(await waitForInstance(started!.id)).toBe("completed");
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  expect((data?.output as any)?.payload).toEqual({ decline_code: "51", retry_after: 3600 });
});

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
        { id: "carry", output: { seen: "$: last_error.data" }, switch: [{ goto: "end" }] },
      ],
      output: "$: outputs.carry",
    },
  });
  expect(putErr).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: name } });
  expect(await waitForInstance(started!.id)).toBe("completed");
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  expect((data?.output as any)?.seen).toEqual({ decline_code: "51", channel: "unknown" });
});

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
          output: { slot: "$: last_error.child_index", why: "$: last_error.data.decline_code" },
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
  expect(data?.output).toEqual({ slot: 0, why: "51" });
});

// The process $defs pool must be baked into the declaration before inference embeds it.
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
        { id: "backoff", output: { wait: "$: last_error.data.retry_after" }, switch: [{ goto: "end" }] },
      ],
      output: "$: outputs.backoff",
    },
  });
  expect(putErr).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: name } });
  expect(await waitForInstance(started!.id)).toBe("completed");
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  expect(data?.output).toEqual({ wait: 3600 });
});

// Past the 2 KiB cutoff the payload is in the object store and must be resolved before the conform.
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
        { id: "keep", output: { trace: "$: last_error.data.trace" }, switch: [{ goto: "end" }] },
      ],
      output: "$: outputs.keep",
    },
  });

  const { data: started } = await client.POST("/instances", { body: { process: name } });
  expect(await waitForInstance(started!.id)).toBe("completed");

  // The proof is on the CHILD's row: error_data has its own field, so the object listing names
  // it rather than a path through `state`.
  const childId = (await childrenOfTask(started!.id, "pay")) as string;
  const { data: kid } = await client.GET("/instances/{id}/detail", { params: { path: { id: childId } } });
  expect(
    (kid!.objects ?? []).some((o: any) => o.path[0] === "error_data"),
    "8 KiB is past the 2 KiB cutoff, so the payload must be externalized",
  ).toBe(true);

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id: started!.id } } });
  await spliceObjects(data);
  expect((data?.output as any)?.trace).toBe(blob);
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
          { case: "true", raise: { code: "gave_up", message: "declined: ${last_error.data.decline_code}" } },
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

// checkDeclaredRaises must hold on every child shape the compatibility family covers.
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
            seen: "$: last_error.data",
            kind: "$: last_error.data.kind",
            declined: "$: last_error.data.decline_code",
            expired: "$: last_error.data.expired_on",
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
    const out = data?.output as any;
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
        { id: "handle", output: { code: "$: last_error.data.decline_code" }, switch: [{ goto: "end" }] },
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
  expect((data?.output as any)?.code, "card_expired carries no decline_code").toBeNull();
});

// Two incompatible payload shapes make case scoping observable. specs/child-error-handling.md M2.
const NAMED = { type: "object", properties: { name: { type: "string" } }, required: ["name"] } as const;
const NUMBERED = { type: "object", properties: { digits: { type: "integer" } }, required: ["digits"] } as const;

async function putCaseScopeChild(name: string) {
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      // Both codes from ONE task: raises(D) is a syntactic scan, so a never-firing case still
      // counts, and a second task nothing routes to would be rejected as unreachable.
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
