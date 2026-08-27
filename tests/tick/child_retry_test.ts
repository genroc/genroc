import { expect, test } from "vitest";
import { useTickEnv } from "./helpers.ts";
import { startMockService } from "../helpers/client.ts";

// Per-slot retry of a raised child: specs/child-error-handling.md §5.5, and the operator's
// counterpart in §12. Ticking rather than polling, and raising from a `switch` rather than
// through a mock service, so every case is deterministic and reads as input → output.
//
// The unit of evidence throughout is `allChildrenOf(parent, task)`: it is unscoped by epoch
// and returns one row per ATTEMPT, so "how many times did this slot run" is a length.
const PORT = 20046;
const ctx = useTickEnv(PORT);

const uid = () => crypto.randomUUID().slice(0, 8);

/** A child that always raises `code`. No action, so nothing external decides the outcome. */
async function defineRaiser(code: string): Promise<string> {
  const name = `raiser_${code}_${uid()}`;
  await ctx.env.define(name, [
    { id: "go", switch: [{ raise: { code, message: `${code} happened` } }] },
  ]);
  return name;
}

/** A child that always completes. */
async function defineCompleter(): Promise<string> {
  const name = `ok_${uid()}`;
  await ctx.env.define(name, [{ id: "go", switch: [{ goto: "end" }] }]);
  return name;
}

/** A parent with one `child` slot and the given on_error rules. */
async function defineCaller(child: string, onError: object[]): Promise<string> {
  const name = `caller_${uid()}`;
  const handlers = [...new Set(onError.map((r) => (r as { goto?: string }).goto).filter(Boolean))]
    .map((g) => ({ id: (g as string).slice(1), output: { routed: (g as string).slice(1) }, switch: [{ goto: "end" }] }));
  await ctx.env.define(name, [
    { id: "call", action: { type: "child", name: child }, on_error: onError, switch: [{ goto: "end" }] },
    ...handlers,
  ]);
  return name;
}

test("a raised slot re-runs once per admitted attempt, then the rule routes", async () => {
  const child = await defineRaiser("svc_down");
  const parent = await defineCaller(child, [
    { code: ["svc_down"], retry: { attempts: 2 }, goto: "$gave_up" },
  ]);

  const id = await ctx.env.start(parent);
  await ctx.env.tickUntilIdle(40);

  // 1 first attempt + 2 admitted retries. Fewer means admission never fired; more means the
  // counter reset each round, which is the shape that never terminates.
  expect(ctx.env.allChildrenOf(id, "call")).toHaveLength(3);
  expect(await ctx.env.status(id)).toBe("completed");
});

test("no retry rule for the raised code means the slot is never re-spawned", async () => {
  const child = await defineRaiser("card_declined");
  // The rule matches and routes, but names no retry: routing is not retrying.
  const parent = await defineCaller(child, [{ code: ["card_declined"], goto: "$gave_up" }]);

  const id = await ctx.env.start(parent);
  await ctx.env.tickUntilIdle(40);

  expect(ctx.env.allChildrenOf(id, "call")).toHaveLength(1);
  expect(await ctx.env.status(id)).toBe("completed");
});

test("the parent keeps the epoch that addresses its batch across a retry round", async () => {
  const child = await defineRaiser("svc_down");
  const parent = await defineCaller(child, [
    { code: ["svc_down"], retry: { attempts: 1 }, goto: "$gave_up" },
  ]);

  const id = await ctx.env.start(parent);
  await ctx.env.tickUntilIdle(40);

  // Every attempt lands in the SAME batch. A moved epoch would orphan the siblings the
  // parent kept, and the collect that followed would merge nothing while reporting success.
  // A child stamps parent_task_epoch from the parent's task_epoch at spawn time, so two
  // attempts sharing a batch IS the proof the parent did not bump between them. (The parent's
  // own epoch does move afterwards, when it routes to $gave_up — that is a task transition,
  // which is exactly what the epoch counts.)
  const attempts = ctx.env.allChildrenOf(id, "call");
  expect(attempts).toHaveLength(2);
  expect(new Set(attempts.map((a) => a.batch)).size, "one batch, two attempts").toBe(1);
});

test("a retry round leaves the parent's own retry_count alone", async () => {
  const child = await defineRaiser("svc_down");
  const parent = await defineCaller(child, [
    { code: ["svc_down"], retry: { attempts: 2 }, goto: "$gave_up" },
  ]);

  const id = await ctx.env.start(parent);
  await ctx.env.tickUntilIdle(40);

  // The budget lives on the child as _spawn_attempt. If it ever moved to the parent,
  // entering the spawn task would zero it and the loop would not terminate.
  expect(await ctx.env.retryCount(id)).toBe(0);
});

test("retired attempts are kept, and only the live one occupies the slot", async () => {
  const child = await defineRaiser("svc_down");
  const parent = await defineCaller(child, [
    { code: ["svc_down"], retry: { attempts: 1 }, goto: "$gave_up" },
  ]);

  const id = await ctx.env.start(parent);
  await ctx.env.tickUntilIdle(40);

  const rows = ctx.env.query<{ id: string; superseded_at: number | null }>(
    "SELECT id, superseded_at FROM process_instances WHERE parent_id = ? ORDER BY created_at",
    id,
  );
  expect(rows.map((r) => (r.superseded_at === null ? "live" : "retired")), "the first attempt is retired, the last is the occupant").toEqual([
    "retired",
    "live",
  ]);
});

/** A parent fanning out to two named children, with the given on_error rules. */
async function defineFanout(slots: Record<string, string>, onError: object[]): Promise<string> {
  const name = `fan_${uid()}`;
  const children: Record<string, object> = {};
  for (const [key, child] of Object.entries(slots)) children[key] = { name: child };
  // Only the handlers the rules actually route to: an unreferenced task is a registration
  // error, which is the reachability rule doing its job.
  const handlers = [...new Set(onError.map((r) => (r as { goto?: string }).goto).filter(Boolean))]
    .map((g) => ({ id: (g as string).slice(1), output: { routed: (g as string).slice(1) }, switch: [{ goto: "end" }] }));
  await ctx.env.define(name, [
    {
      id: "fanout",
      action: { type: "child_map", children },
      on_error: onError,
      switch: [{ goto: "end" }],
    },
    ...handlers,
  ]);
  return name;
}

test("only the raised slot is replaced — a completed sibling is never re-run", async () => {
  const parent = await defineFanout(
    { good: await defineCompleter(), bad: await defineRaiser("svc_down") },
    [{ code: ["svc_down"], retry: { attempts: 2 }, goto: "$gave_up_a" }],
  );

  const id = await ctx.env.start(parent);
  await ctx.env.tickUntilIdle(60);

  // 1 completed sibling + 3 attempts at the raised slot. A batch-wide re-spawn would have
  // re-run `good` too, and a shared budget could not have kept them independent.
  const rows = ctx.env.query<{ process_name: string }>(
    "SELECT process_name FROM process_instances WHERE parent_id = ?",
    id,
  );
  const runs = rows.reduce<Record<string, number>>((acc, r) => {
    const slot = r.process_name.startsWith("ok_") ? "good" : "bad";
    acc[slot] = (acc[slot] ?? 0) + 1;
    return acc;
  }, {});
  expect(runs).toEqual({ good: 1, bad: 3 });
});

test("a defect in the batch cancels the retry that had not happened yet", async () => {
  const panicker = `panicker_${uid()}`;
  await ctx.env.define(panicker, [
    { id: "go", switch: [{ panic: { code: "contract_broken", message: "broken" } }] },
  ]);
  const parent = await defineFanout(
    { a_raiser: await defineRaiser("svc_down"), b_panicker: panicker },
    [{ code: ["svc_down"], retry: { attempts: 3 }, goto: "$gave_up_a" }],
  );

  const id = await ctx.env.start(parent);
  await ctx.env.tickUntilIdle(60);

  // The defect poisons the parent, so it settles without resolving and admission never runs.
  // Retrying eagerly — the moment a slot raised — would have spent attempts, and their side
  // effects, on a batch already doomed. specs/child-error-handling.md §5.4, E3.
  expect(ctx.env.allChildrenOf(id, "fanout")).toHaveLength(2);
  expect(await ctx.env.status(id)).toBe("failed");
});

test("a raise no rule would retry waits for its retrying siblings before routing", async () => {
  const parent = await defineFanout(
    { a_declined: await defineRaiser("card_declined"), b_down: await defineRaiser("svc_down") },
    [
      { code: ["card_declined"], goto: "$gave_up_a" },
      { code: ["svc_down"], retry: { attempts: 2 }, goto: "$gave_up_b" },
    ],
  );

  const id = await ctx.env.start(parent);
  await ctx.env.tickUntilIdle(60);

  // `a_declined` is raised[0] in slot order and never retries, but it does not route until
  // `b_down` has spent its budget: the batch is the unit, so routing waits for the round.
  const rows = ctx.env.query<{ error_code: string }>(
    "SELECT error_code FROM process_instances WHERE parent_id = ?",
    id,
  );
  const attempts = rows.reduce<Record<string, number>>((acc, r) => {
    acc[r.error_code] = (acc[r.error_code] ?? 0) + 1;
    return acc;
  }, {});
  expect(attempts, "the un-retryable slot ran once; the retryable one spent its budget").toEqual({
    card_declined: 1,
    svc_down: 3,
  });
  const { data } = await ctx.env.client.GET("/instances/{id}/detail", { params: { path: { id } } });
  expect((data?.state?.outputs as any)?.gave_up_a, "and then raised[0]'s rule routes").toBeDefined();
});

// ---- §12: the operator's `retry` -------------------------------------------------------

test("retry re-spawns a raised child rather than reviving it", async () => {
  const child = await defineRaiser("svc_down");
  const parent = await defineCaller(child, []); // no rule: an unmatched raise fails the parent

  const id = await ctx.env.start(parent);
  await ctx.env.tickUntilIdle(40);
  expect(await ctx.env.status(id)).toBe("failed");
  expect(ctx.env.allChildrenOf(id, "call")).toHaveLength(1);

  await ctx.env.retry(id);
  await ctx.env.tickUntilIdle(40);

  // A second ROW, not the same one run again: a raise concluded at a task boundary, so
  // reviving would only re-run the switch that decided to raise.
  const attempts = ctx.env.allChildrenOf(id, "call");
  expect(attempts, "the raised attempt is replaced, not restarted").toHaveLength(2);
  expect(new Set(attempts.map((a) => a.batch)).size, "the replacement joins the same batch").toBe(1);
});

test("retry grants one extra attempt, not a fresh budget", async () => {
  const child = await defineRaiser("svc_down");
  // A verb-less rule: spend the budget, then fail with the child's code. That leaves the tree
  // failed with the slot's count AT its limit, which is what the operator then overrides.
  const parent = await defineCaller(child, [{ code: ["svc_down"], retry: { attempts: 1 } }]);

  const id = await ctx.env.start(parent);
  await ctx.env.tickUntilIdle(40);
  expect(ctx.env.allChildrenOf(id, "call"), "1 attempt + 1 admitted retry").toHaveLength(2);
  expect(await ctx.env.status(id)).toBe("failed");

  await ctx.env.retry(id);
  await ctx.env.tickUntilIdle(40);

  // 3 = the operator's one override. 4 would mean the replacement came back on a zeroed
  // counter and the definition's budget ran a second time — a retry command silently
  // multiplying the policy.
  expect(ctx.env.allChildrenOf(id, "call"), "one more run, then the count declines it again").toHaveLength(3);
  expect(await ctx.env.status(id)).toBe("failed");
});

test("a raised ROOT is refused — there is no parent to re-spawn it from", async () => {
  const root = await defineRaiser("svc_down");
  const id = await ctx.env.start(root);
  await ctx.env.tickUntilIdle(20);
  expect(await ctx.env.status(id)).toBe("raised");

  const { error } = await ctx.env.client.POST("/instances/{id}/retry", { params: { path: { id } } });
  expect(error, "a raise is retried by re-spawning it from its parent, and a root has none").toBeDefined();
  expect(JSON.stringify(error)).toContain("raised");
});

// ---- the conform that decides which rule a slot matches ---------------------------------

/** A child that raises `card_declined` carrying the given payload. */
async function defineDecliner(data: unknown): Promise<string> {
  const name = `decliner_${uid()}`;
  await ctx.env.define(name, [
    { id: "go", switch: [{ raise: { code: "card_declined", message: "declined", data } }] },
  ]);
  return name;
}

const DECLINE_SHAPE = {
  type: "object",
  properties: { decline_code: { type: "string" } },
  required: ["decline_code"],
};

test("each slot is conformed, so a bad payload takes its own code away", async () => {
  const conforms = await defineDecliner({ decline_code: "51" });
  const doesNot = await defineDecliner("just a string");
  const name = `conform_${uid()}`;
  await ctx.env.define(name, [
    {
      id: "fanout",
      action: {
        type: "child_map",
        children: {
          a_ok: { name: conforms, raises: { card_declined: DECLINE_SHAPE } },
          b_bad: { name: doesNot, raises: { card_declined: DECLINE_SHAPE } },
        },
      },
      on_error: [
        { code: ["card_declined"], retry: { attempts: 1 }, goto: "$handled" },
        { code: ["output.invalid"], goto: "$invalid" },
      ],
      switch: [{ goto: "end" }],
    },
    { id: "handled", output: { routed: "declined" }, switch: [{ goto: "end" }] },
    { id: "invalid", output: { routed: "invalid" }, switch: [{ goto: "end" }] },
  ]);

  const id = await ctx.env.start(name);
  await ctx.env.tickUntilIdle(60);

  // `a_ok` is raised[0] in slot order. If only IT were conformed — the shipped behaviour
  // before §5.5 — `b_bad` would still have read as card_declined and retried alongside it.
  // Its payload failing the declaration is what replaces its code with output.invalid, and
  // the rule that catches output.invalid names no retry.
  const rows = ctx.env.query<{ process_name: string }>(
    "SELECT process_name FROM process_instances WHERE parent_id = ?",
    id,
  );
  const runs = rows.reduce<Record<string, number>>((acc, r) => {
    const slot = r.process_name === conforms ? "a_ok" : "b_bad";
    acc[slot] = (acc[slot] ?? 0) + 1;
    return acc;
  }, {});
  expect(runs, "the conforming slot spends its budget; the mismatched one never matches the rule that has one").toEqual({
    a_ok: 2,
    b_bad: 1,
  });
});

test("a replacement carries the input its attempt was given", async () => {
  const child = `echo_${uid()}`;
  await ctx.env.define(child, [
    { id: "go", switch: [{ raise: { code: "svc_down", message: "down" } }] },
  ]);
  const parent = `input_${uid()}`;
  await ctx.env.define(parent, [
    {
      id: "call",
      action: { type: "child", name: child, input: { ticket: "abc-123" } },
      on_error: [{ code: ["svc_down"], retry: { attempts: 1 } }],
      switch: [{ goto: "end" }],
    },
  ]);

  const id = await ctx.env.start(parent);
  await ctx.env.tickUntilIdle(40);

  // A re-spawn re-sends the stored input rather than rebuilding it. This pins that the input
  // TRAVELS — dropping it would leave the replacement running on nothing. It cannot tell
  // copying from re-evaluating, which only differ when the parent's context has moved, and
  // the one live input (`config`) is fixed for the life of the server.
  const inputs = ctx.env.query<{ input_data: string }>(
    "SELECT input_data FROM process_instances WHERE parent_id = ? ORDER BY created_at",
    id,
  );
  expect(inputs).toHaveLength(2);
  expect(JSON.parse(inputs[0].input_data)).toEqual({ ticket: "abc-123" });
  expect(inputs[1].input_data, "the replacement is given what the attempt it replaces was given").toBe(
    inputs[0].input_data,
  );
});

test("budgets multiply: a parent's retry runs the child's own budget again", async () => {
  const mock = await startMockService(0, { statusCode: 503 });
  try {
    const child = `nested_${uid()}`;
    await ctx.env.define(child, [
      {
        id: "call",
        action: { type: "fetch", url: `http://localhost:${mock.port}/x`, responses: { "200": {} } },
        timeout: 2000,
        // The child spends its OWN budget first, then concludes with a raise.
        on_error: [
          { code: ["http.5%"], retry: { attempts: 1 }, raise: { code: "svc_down", message: "down" } },
        ],
        switch: [{ goto: "end" }],
      },
    ]);
    const parent = `nesting_${uid()}`;
    await ctx.env.define(parent, [
      {
        id: "call",
        action: { type: "child", name: child },
        on_error: [{ code: ["svc_down"], retry: { attempts: 1 } }],
        switch: [{ goto: "end" }],
      },
    ]);

    const id = await ctx.env.start(parent);
    await ctx.env.tickUntilIdle(60);

    // 2 child instances × 2 attempts each. Nesting means the budgets compose, which is what
    // anyone would expect on reflection and nobody expects in the moment — a parent's
    // `retry: 1` over a child's `retry: 1` is FOUR calls to the thing that is down.
    expect(ctx.env.allChildrenOf(id, "call"), "the parent spent its budget").toHaveLength(2);
    expect(mock.requestCount(), "each attempt at the slot ran the child's whole budget").toBe(4);
  } finally {
    await mock.stop();
  }
});

// ---- reviving onto an error handler -----------------------------------------------------

test("a handler task revived by retry still has the error it was reached through", async () => {
  const uid2 = uid();
  const child = `handler_child_${uid2}`;
  const parent = `handler_parent_${uid2}`;
  await ctx.env.define(child, [
    { id: "go", switch: [{ raise: { code: "boom", message: "kaboom", data: { name: "Error" } } }] },
  ]);
  await ctx.env.define(parent, [
    {
      id: "call",
      action: {
        type: "child",
        name: child,
        raises: { boom: { type: "object", properties: { name: { type: "string" } }, required: ["name"] } },
      },
      on_error: [{ code: ["boom"], goto: "$classify" }],
      switch: [{ goto: "end" }],
    },
    {
      // Reachable ONLY through on_error, so mustErr/mayErr promises `error` exists here.
      id: "classify",
      switch: [
        { case: 'error.data.name == "Transient"', goto: "$call" },
        { panic: { code: "broken", message: "threw ${error.data.name}" } },
      ],
    },
  ]);

  const id = await ctx.env.start(parent);
  await ctx.env.tickUntilIdle(30);
  expect(await ctx.env.status(id)).toBe("failed");

  await ctx.env.retry(id);
  await ctx.env.tickUntilIdle(30);

  // Retry revives the instance AT `classify`. Clearing the caught error there would hand the
  // handler a null the analysis says it can never see: it takes the wrong branch, and the
  // panic message — which interpolates `error` — degrades to its own source text.
  const { data } = await ctx.env.client.GET("/instances/{id}", { params: { path: { id } } });
  expect(data!.error_message, "the message renders, so the handler still had its error").toBe("threw Error");
  expect(String(data!.error_message)).not.toContain("${");
});

// ---- the workflow the reversal exists for: fix, apply, upgrade, retry --------------------

test("upgrade then retry runs the child with the NEW input, not the one it failed on", async () => {
  const uid2 = uid();
  const child = `upg_child_${uid2}`;
  const parent = `upg_parent_${uid2}`;
  await ctx.env.define(child, [
    { id: "go", switch: [{ raise: { code: "svc_down", message: "down" } }] },
  ]);
  // v1 passes the broken "code". No on_error, so the unmatched raise fails the parent
  // STANDING ON the child call — which is what leaves the batch in retry's reach.
  const callTask = (codeVal: string) => ({
    id: "call",
    action: { type: "child", name: child, input: { code: codeVal } },
    switch: [{ goto: "end" }],
  });
  await ctx.env.define(parent, [callTask("broken-v1")]);

  const id = await ctx.env.start(parent);
  await ctx.env.tickUntilIdle(30);
  expect(await ctx.env.status(id)).toBe("failed");

  // The fix, published as a new version — exactly what `$import`ing a corrected script does.
  await ctx.env.define(parent, [callTask("fixed-v2")]);
  const { error: upErr } = await ctx.env.client.POST("/instances/{id}/upgrade", {
    params: { path: { id } },
    body: { from_version: 0, to_version: 2 }, // 0 skips the from-version assertion
  });
  expect(upErr).toBeUndefined();

  await ctx.env.retry(id);
  await ctx.env.tickUntilIdle(30);

  // The replacement's input is REBUILT from the parent as it now stands. Re-sending what the
  // failed attempt was given would hand it "broken-v1" forever, and no amount of fixing,
  // applying and upgrading could ever reach the child.
  const inputs = ctx.env
    .query<{ input_data: string }>(
      "SELECT input_data FROM process_instances WHERE parent_id = ? ORDER BY created_at",
      id,
    )
    .map((r) => JSON.parse(r.input_data).code);
  expect(inputs, "the first attempt ran the broken input; its replacement runs the fixed one").toEqual([
    "broken-v1",
    "fixed-v2",
  ]);
});
