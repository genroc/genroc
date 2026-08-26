import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// The error channel on an external task: a caller submits an `error` instead of a `result`
// — same endpoint, same token — and it is routed through on_error like any call error.
// One submission carries one outcome. See specs/external-task-queue.md §"The error channel".

// waitForQueued polls the queue until this process's task is parked, and returns the entry —
// the token is what a worker answers with, on either channel.
async function waitForQueued(process: string, timeoutMs = 20_000): Promise<any> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const { data, error } = await client.GET("/external-tasks", {
      params: { query: { process } },
    });
    if (error) throw new Error(`list external tasks failed: ${JSON.stringify(error)}`);
    const items = ((data as any)?.items ?? []) as any[];
    if (items.length) return items[0];
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error(`no external task queued for ${process} in time`);
}

// A process that parks on `work`, declares one payload shape, and routes the code to a task
// that reads error.data. `extra` merges into the work task so a case can add only_once etc.
async function define(name: string, extra: Record<string, unknown> = {}) {
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "work",
          action: {
            type: "external" as const,
            input: { job: "compute" },
            raises: {
              limit_exceeded: {
                type: "object",
                properties: { limit: { type: "number" } },
                required: ["limit"],
              },
              // null declares a code that carries nothing — the spelling for a failure with
              // no payload, distinct from {} (an opaque one). It is a schema position, so
              // "no schema" is the absence of one, not a boolean.
              worker_crashed: null,
            },
          },
          on_error: [
            { code: ["limit_exceeded"], goto: "$over_limit" },
            { code: ["%"], goto: "$other" },
          ],
          switch: [{ goto: "end" }],
          ...extra,
        },
        {
          id: "over_limit",
          output: { route: "limit", limit: "$: error.data.limit", msg: "$: error.message" },
          switch: [{ goto: "end" }],
        },
        { id: "other", output: { route: "other", code: "$: error.code" }, switch: [{ goto: "end" }] },
      ],
    },
  });
  if (error) throw new Error(`put definition failed: ${JSON.stringify(error)}`);
}

async function start(name: string): Promise<string> {
  const { data, error } = await client.POST("/instances", { body: { process: name } });
  if (error) throw new Error(`start failed: ${JSON.stringify(error)}`);
  return data!.id;
}

// Each terminal task projects into outputs.<task>, so which key is present is itself the
// assertion that the intended on_error rule fired.
async function outputsOf(id: string): Promise<any> {
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  return (data as any)?.state?.outputs ?? {};
}

test("a declared code routes through on_error and carries its payload as error.data", async () => {
  const name = `ext_fail_declared_${crypto.randomUUID()}`;
  await define(name);
  const id = await start(name);
  const queued = await waitForQueued(name);

  // The queue publishes the shapes a worker may answer with, on both channels.
  expect(queued.raises?.limit_exceeded).toBeTruthy();

  const { error } = await client.POST("/external-tasks/resolve", {
    body: {
      token: queued.token,
      error: {
        code: "limit_exceeded",
        message: "the amount is over the approval limit",
        data: { limit: 1000 },
      },
    },
  });
  expect(error, `fail was rejected: ${JSON.stringify(error)}`).toBeUndefined();

  expect(await waitForInstance(id)).toBe("completed");
  expect((await outputsOf(id)).over_limit).toEqual({
    route: "limit",
    limit: 1000,
    msg: "the amount is over the approval limit",
  });
});

test("a code outside raises is refused — raises is the closed set a worker may send", async () => {
  const name = `ext_fail_undeclared_${crypto.randomUUID()}`;
  await define(name);
  await start(name);
  const queued = await waitForQueued(name);

  const { error } = await client.POST("/external-tasks/resolve", {
    body: { token: queued.token, error: { code: "limit_exceded", message: "typo", data: { limit: 1 } } },
  });
  // The whole point of closing the set: a typo cannot quietly fall through to the catch-all
  // rule. Nothing about a worker is knowable at registration, so this is the only place it
  // can be caught at all.
  expect(error, "a code outside raises must be refused").toBeTruthy();
  expect(JSON.stringify(error)).toContain("limit_exceeded"); // the message lists what is accepted
});

test("a task with no raises has no error channel", async () => {
  const name = `ext_fail_noraises_${crypto.randomUUID()}`;
  const { error: defErr } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "work",
          action: { type: "external" as const, input: {} },
          on_error: [{ code: ["%"], goto: "$done" }],
          switch: [{ goto: "end" }],
        },
        { id: "done", output: { route: "done" }, switch: [{ goto: "end" }] },
      ],
    },
  });
  expect(defErr, `put definition failed: ${JSON.stringify(defErr)}`).toBeUndefined();
  await start(name);
  const queued = await waitForQueued(name);

  const { error } = await client.POST("/external-tasks/resolve", {
    body: { token: queued.token, error: { code: "anything", message: "m" } },
  });
  expect(error, "a task declaring no raises must refuse every failure").toBeTruthy();
  expect(JSON.stringify(error)).toContain("declares no raises");
});

test("`raises: {code: null}` accepts a failure with no payload and leaves error.data absent", async () => {
  const name = `ext_fail_nodata_${crypto.randomUUID()}`;
  await define(name);
  const id = await start(name);
  const queued = await waitForQueued(name);

  const { error } = await client.POST("/external-tasks/resolve", {
    body: { token: queued.token, error: { code: "worker_crashed", message: "the worker died" } },
  });
  expect(error, `fail was rejected: ${JSON.stringify(error)}`).toBeUndefined();

  expect(await waitForInstance(id)).toBe("completed");
  expect((await outputsOf(id)).other).toEqual({ route: "other", code: "worker_crashed" });

  // Declared-as-carrying-nothing must leave the slot ABSENT, not null: absence is what the
  // validator infers for the code, and a context richer than its type is how an expression
  // comes to read a slot the next reader cannot.
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  const err = (data as any)?.state?.error;
  expect(err?.code).toBe("worker_crashed");
  expect(Object.keys(err ?? {})).not.toContain("data");
});

test("a payload sent for a code declared null is refused", async () => {
  const name = `ext_fail_nodata_payload_${crypto.randomUUID()}`;
  await define(name);
  await start(name);
  const queued = await waitForQueued(name);

  const { error } = await client.POST("/external-tasks/resolve", {
    body: { token: queued.token, error: { code: "worker_crashed", message: "m", data: { any: 1 } } },
  });
  expect(error, "declaring a code carries nothing must reject a payload for it").toBeTruthy();
});

test("a payload that does not match the declared shape is refused, and the task stays parked", async () => {
  const name = `ext_fail_badpayload_${crypto.randomUUID()}`;
  await define(name);
  const id = await start(name);
  const queued = await waitForQueued(name);

  const { error } = await client.POST("/external-tasks/resolve", {
    body: { token: queued.token, error: { code: "limit_exceeded", message: "over", data: { limit: "lots" } } },
  });
  expect(error, "a payload violating raises[code] must be refused at submission").toBeTruthy();

  // Refusing at submission is only useful if the work is still answerable afterwards —
  // otherwise a typo would strand the instance.
  const retry = await waitForQueued(name);
  expect(retry.token).toBe(queued.token);
  const { error: ok } = await client.POST("/external-tasks/resolve", {
    body: { token: queued.token, error: { code: "limit_exceeded", message: "over", data: { limit: 5 } } },
  });
  expect(ok, `the corrected submission was rejected: ${JSON.stringify(ok)}`).toBeUndefined();
  expect(await waitForInstance(id)).toBe("completed");
  expect((await outputsOf(id)).over_limit.limit).toBe(5);
});

test("a dotted code is refused — a worker cannot impersonate an engine code", async () => {
  const name = `ext_fail_dotted_${crypto.randomUUID()}`;
  await define(name);
  await start(name);
  const queued = await waitForQueued(name);

  for (const code of ["http.500", "external.timeout", "only_once.interrupted"]) {
    const { error } = await client.POST("/external-tasks/resolve", {
      body: { token: queued.token, error: { code, message: "nope" } },
    });
    expect(error, `${code} must be refused: engine codes are not a worker's to send`).toBeTruthy();
  }
  // And a code that is merely malformed rather than dotted.
  const { error } = await client.POST("/external-tasks/resolve", {
    body: { token: queued.token, error: { code: "Not Valid", message: "nope" } },
  });
  expect(error, "a non lower_snake_case code must be refused").toBeTruthy();
});

test("a failure submitted while paused is delivered on resume, not discarded", async () => {
  const name = `ext_fail_paused_${crypto.randomUUID()}`;
  await define(name);
  const id = await start(name);
  const queued = await waitForQueued(name);

  const { error: pauseErr } = await client.POST("/instances/{id}/pause", {
    params: { path: { id } },
  });
  expect(pauseErr, `pause failed: ${JSON.stringify(pauseErr)}`).toBeUndefined();

  // A pause suspends execution, not delivery. Refusing here would strand work the worker
  // has already done, and on an only_once task the external.timeout that follows can never
  // be retried. specs/external-task-queue.md §Pause.
  const { error } = await client.POST("/external-tasks/resolve", {
    body: { token: queued.token, error: { code: "limit_exceeded", message: "over", data: { limit: 7 } } },
  });
  expect(error, `a failure submitted to a paused instance was refused: ${JSON.stringify(error)}`).toBeUndefined();

  await client.POST("/instances/{id}/resume", { params: { path: { id } } });
  expect(await waitForInstance(id)).toBe("completed");
  expect((await outputsOf(id)).over_limit.limit).toBe(7);
});

test("a result submitted while paused is delivered on resume, not discarded", async () => {
  const name = `ext_resolve_paused_${crypto.randomUUID()}`;
  const { error: defErr } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "work",
          action: { type: "external" as const, input: { job: "compute" }, result_schema: {} },
          output: "$: self.result",
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(defErr, `put definition failed: ${JSON.stringify(defErr)}`).toBeUndefined();
  const id = await start(name);
  const queued = await waitForQueued(name);

  await client.POST("/instances/{id}/pause", { params: { path: { id } } });
  const { error } = await client.POST("/external-tasks/resolve", {
    body: { token: queued.token, result: { done: true } },
  });
  expect(error, `a result submitted to a paused instance was refused: ${JSON.stringify(error)}`).toBeUndefined();

  await client.POST("/instances/{id}/resume", { params: { path: { id } } });
  expect(await waitForInstance(id)).toBe("completed");
  expect((await outputsOf(id)).work).toEqual({ done: true });
});

test("on an only_once task a retry on a worker-reported code is refused at registration", async () => {
  const name = `ext_fail_only_once_${crypto.randomUUID()}`;
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "work",
          action: { type: "external" as const, input: { job: "compute" }, raises: { worker_failed: null } },
          only_once: true,
          // A worker that answered `fail` reached the work, so the code's default
          // classification is "potentially reached" — the retry must be refused unless the
          // author asserts otherwise. Nothing about the code being authored rather than an
          // engine code exempts it.
          on_error: [{ code: ["worker_failed"], retry: { attempts: 3, delay: 50 }, goto: "$gave_up" }],
          switch: [{ goto: "end" }],
        },
        { id: "gave_up", output: { route: "gave_up" }, switch: [{ goto: "end" }] },
      ],
    },
  });
  expect(error, "only_once must refuse a retry on a code that may have executed").toBeTruthy();
  expect(JSON.stringify(error)).toContain("not_reached");
});

test("not_reached:true lets an only_once task re-arm, and the re-arming gets a fresh token", async () => {
  const name = `ext_fail_not_reached_${crypto.randomUUID()}`;
  const { error: defErr } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "work",
          action: { type: "external" as const, input: { job: "compute" }, raises: { never_started: null } },
          only_once: true,
          // The author asserting the worker never started the work is what buys the retry
          // back — and it must name the exact code, since an assertion is about one error.
          on_error: [
            {
              code: ["never_started"],
              not_reached: true,
              retry: { attempts: 1, delay: 50 },
              goto: "$gave_up",
            },
          ],
          switch: [{ goto: "end" }],
        },
        { id: "gave_up", output: { route: "gave_up" }, switch: [{ goto: "end" }] },
      ],
    },
  });
  expect(defErr, `put definition failed: ${JSON.stringify(defErr)}`).toBeUndefined();

  const id = await start(name);
  const first = await waitForQueued(name);
  await client.POST("/external-tasks/resolve", {
    body: { token: first.token, error: { code: "never_started", message: "the worker never picked it up" } },
  });

  // The retry re-arms the task, which is a new OCCURRENCE: the token must change, or the
  // first worker's stale answer would be accepted against the second arming.
  const deadline = Date.now() + 20_000;
  let second: any;
  while (Date.now() < deadline) {
    const { data } = await client.GET("/external-tasks", { params: { query: { process: name } } });
    const hit = ((data as any)?.items ?? [])[0];
    if (hit && hit.token !== first.token) {
      second = hit;
      break;
    }
    await new Promise((r) => setTimeout(r, 50));
  }
  expect(second, "the retry did not re-arm the external task").toBeTruthy();

  // The retry budget is spent, so this one routes.
  await client.POST("/external-tasks/resolve", {
    body: { token: second.token, error: { code: "never_started", message: "still nothing" } },
  });
  expect(await waitForInstance(id)).toBe("completed");
  expect((await outputsOf(id)).gave_up).toEqual({ route: "gave_up" });

  // And the first arming's token is dead: it named an occurrence that has been superseded.
  const { error: stale } = await client.POST("/external-tasks/resolve", {
    body: { token: first.token, error: { code: "never_started", message: "late" } },
  });
  expect(stale, "a token from a superseded arming must be refused").toBeTruthy();
});

// The point of one outcome envelope: both addressing modes carry both channels. Before the
// unification /instances/{id}/signal could report success and not failure, and the buffer had
// a single `result` column that could not hold the other half at all.

test("signal delivers a failure to an armed task, by instance id", async () => {
  const name = `ext_signal_fail_${crypto.randomUUID()}`;
  await define(name);
  const id = await start(name);
  await waitForQueued(name); // armed

  const { data, error } = await client.POST("/instances/{id}/signal", {
    params: { path: { id } },
    body: { task_id: "work", error: { code: "limit_exceeded", message: "over", data: { limit: 3 } } },
  });
  expect(error, `signal was rejected: ${JSON.stringify(error)}`).toBeUndefined();
  expect((data as any)?.delivered, "an armed task takes the failure immediately").toBe(true);

  expect(await waitForInstance(id)).toBe("completed");
  expect((await outputsOf(id)).over_limit.limit).toBe(3);
});

test("a failure signalled BEFORE the task arms is buffered, then routed when it arms", async () => {
  const name = `ext_signal_fail_buffered_${crypto.randomUUID()}`;
  const { error: defErr } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        // A delay in front of the external task, so the signal lands while `work` is not yet
        // armed — the branch that has to buffer.
        { id: "wait", action: { type: "delay" as const, for: 1500 }, switch: [{ goto: "next" }] },
        {
          id: "work",
          action: {
            type: "external" as const,
            input: {},
            raises: { upstream_failed: { type: "object", properties: { why: { type: "string" } } } },
          },
          on_error: [{ code: ["upstream_failed"], goto: "$handled" }],
          switch: [{ goto: "end" }],
        },
        { id: "handled", output: { why: "$: error.data.why" }, switch: [{ goto: "end" }] },
      ],
    },
  });
  expect(defErr, `put definition failed: ${JSON.stringify(defErr)}`).toBeUndefined();

  const id = await start(name);
  const { data, error } = await client.POST("/instances/{id}/signal", {
    params: { path: { id } },
    body: { task_id: "work", error: { code: "upstream_failed", message: "the job died", data: { why: "oom" } } },
  });
  expect(error, `signal was rejected: ${JSON.stringify(error)}`).toBeUndefined();
  // Buffered, not delivered: the whole reason process_signals.result had to become `outcome`.
  expect((data as any)?.buffered, "an unarmed task must buffer the failure").toBe(true);

  expect(await waitForInstance(id, 20_000)).toBe("completed");
  expect((await outputsOf(id)).handled).toEqual({ why: "oom" });
});

test("a submission carries one outcome, not both", async () => {
  const name = `ext_both_${crypto.randomUUID()}`;
  await define(name);
  await start(name);
  const queued = await waitForQueued(name);

  const { error } = await client.POST("/external-tasks/resolve", {
    body: {
      token: queued.token,
      result: { ok: true },
      error: { code: "limit_exceeded", message: "over", data: { limit: 1 } },
    },
  });
  expect(error, "result and error together must be refused").toBeTruthy();
});

// Validation is shared (api.buildOutcome), but the two modes reach it differently: resolve
// validates against the instance's CURRENT task, signal against the task it NAMES, looked up
// in the pinned definition. So signal can validate against the wrong task and resolve cannot —
// which is what these pin, rather than re-running the shared rules.

test("signal validates the failure against the task it names, not another task's raises", async () => {
  const name = `sig_wrong_task_${crypto.randomUUID()}`;
  const { error: defErr } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "first",
          action: { type: "external" as const, input: {}, raises: { only_on_first: null } },
          on_error: [{ code: ["%"], goto: "$done" }],
          switch: [{ goto: "next" }],
        },
        {
          id: "second",
          action: { type: "external" as const, input: {}, raises: { only_on_second: null } },
          on_error: [{ code: ["%"], goto: "$done" }],
          switch: [{ goto: "end" }],
        },
        { id: "done", switch: [{ goto: "end" }] },
      ],
    },
  });
  expect(defErr, `put definition failed: ${JSON.stringify(defErr)}`).toBeUndefined();
  const id = await start(name);
  await waitForQueued(name); // parked on `first`

  // `only_on_first` is declared by the CURRENT task but not by the one being signalled, so
  // addressing `second` with it must be refused: signal's closed set is the named task's.
  const { error: wrong } = await client.POST("/instances/{id}/signal", {
    params: { path: { id } },
    body: { task_id: "second", error: { code: "only_on_first", message: "m" } },
  });
  expect(wrong, "signal must validate against the task it names, not the current one").toBeTruthy();
  expect(JSON.stringify(wrong)).toContain("only_on_second"); // the message lists that task's set

  // And the converse: the named task's own code is accepted, buffered for when it arms.
  const { data, error } = await client.POST("/instances/{id}/signal", {
    params: { path: { id } },
    body: { task_id: "second", error: { code: "only_on_second", message: "m" } },
  });
  expect(error, `the named task's own code was refused: ${JSON.stringify(error)}`).toBeUndefined();
  expect((data as any)?.buffered).toBe(true);
});

test("signal runs the same outcome validation as resolve", async () => {
  const name = `sig_validation_${crypto.randomUUID()}`;
  await define(name);
  const id = await start(name);
  await waitForQueued(name);

  const cases: Array<[string, Record<string, unknown>]> = [
    ["an undeclared code", { error: { code: "not_declared", message: "m" } }],
    ["a dotted code", { error: { code: "http.500", message: "m" } }],
    ["a missing message", { error: { code: "limit_exceeded" } }],
    ["a payload violating raises[code]", { error: { code: "limit_exceeded", message: "m", data: { limit: "lots" } } }],
    ["a payload for a null-declared code", { error: { code: "worker_crashed", message: "m", data: { x: 1 } } }],
    ["both outcomes at once", { result: { ok: true }, error: { code: "limit_exceeded", message: "m", data: { limit: 1 } } }],
  ];
  for (const [label, body] of cases) {
    const { error } = await client.POST("/instances/{id}/signal", {
      params: { path: { id } },
      body: { task_id: "work", ...body } as never,
    });
    expect(error, `signal must refuse ${label}, as resolve does`).toBeTruthy();
  }

  // Still answerable afterwards: every refusal above is a 400 that left the task parked.
  const { error: ok } = await client.POST("/instances/{id}/signal", {
    params: { path: { id } },
    body: { task_id: "work", error: { code: "limit_exceeded", message: "m", data: { limit: 9 } } },
  });
  expect(ok, `the valid submission was rejected: ${JSON.stringify(ok)}`).toBeUndefined();
  expect(await waitForInstance(id)).toBe("completed");
  expect((await outputsOf(id)).over_limit.limit).toBe(9);
});

test("signal validates a result against result_schema", async () => {
  const name = `sig_result_schema_${crypto.randomUUID()}`;
  const { error: defErr } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "work",
          action: {
            type: "external" as const,
            input: {},
            result_schema: {
              type: "object",
              properties: { approved: { type: "boolean" } },
              required: ["approved"],
            },
          },
          output: "$: self.result",
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(defErr, `put definition failed: ${JSON.stringify(defErr)}`).toBeUndefined();
  const id = await start(name);
  await waitForQueued(name);

  const { error } = await client.POST("/instances/{id}/signal", {
    params: { path: { id } },
    body: { task_id: "work", result: { approved: "yes" } },
  });
  expect(error, "signal must conform a result to result_schema, as resolve does").toBeTruthy();

  const { error: ok } = await client.POST("/instances/{id}/signal", {
    params: { path: { id } },
    body: { task_id: "work", result: { approved: true } },
  });
  expect(ok, `the valid result was rejected: ${JSON.stringify(ok)}`).toBeUndefined();
  expect(await waitForInstance(id)).toBe("completed");
  expect((await outputsOf(id)).work).toEqual({ approved: true });
});
