import { parkedInProcess, parkedTask } from "../helpers/external.ts";
import { expect, test } from "vitest";
import { client, outputsOf, startInstance, waitForInstance } from "../helpers/client.ts";

// The pull half of the external-task queue: claim, renew, release, answer. specs/external-task-queue.md.

function workThenFailed(work = "work") {
  return [
    {
      id: work,
      action: {
        type: "external" as const,
        input: { job: "compute" },
        result_schema: {},
        raises: { worker_failed: null },
      },
      output: "$: self.result",
      on_error: [{ code: ["worker_failed"], goto: "$failed" }],
      switch: [{ goto: "end" }],
    },
    { id: "failed", output: { route: "failed" }, switch: [{ goto: "end" }] },
  ];
}

async function define(name: string, tasks: unknown[] = workThenFailed()) {
  const { error } = await client.PUT("/definitions", { body: { name, tasks: tasks as never } });
  if (error) throw new Error(`put definition failed: ${JSON.stringify(error)}`);
}

async function claim(worker: string, process: string, opts: Record<string, unknown> = {}) {
  const { data, error } = await client.POST("/external-tasks/claim", {
    body: { worker_id: worker, process, ...opts } as never,
  });
  if (error) throw new Error(`claim failed: ${JSON.stringify(error)}`);
  return ((data as any)?.items ?? []) as any[];
}

// A fresh instance takes a tick to reach its external task.
async function claimWhenReady(worker: string, process: string, opts: Record<string, unknown> = {}) {
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    const got = await claim(worker, process, opts);
    if (got.length) return got;
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error(`nothing claimable for ${process} in time`);
}

test("a claim leases the task, and the granted token answers it", async () => {
  const name = `claim_basic_${crypto.randomUUID()}`;
  await define(name);
  const id = await startInstance(name);

  const [job] = await claimWhenReady("worker-1", name);
  // The claim token is three-part: instance, arming, grant. The queue's own two-part token
  // names no grant and is refused while this one is live.
  expect(job.token.split(".").length).toBe(3);
  expect(job.task).toBe("work");
  expect(job.external_input).toEqual({ job: "compute" });

  const { error } = await client.POST("/external-tasks/resolve", {
    body: { token: job.token, result: { priced: 42 } },
  });
  expect(error, `answering under the claim token was refused: ${JSON.stringify(error)}`).toBeUndefined();
  expect(await waitForInstance(id)).toBe("completed");
  expect((await outputsOf(id)).work).toEqual({ priced: 42 });
});

test("a live claim is not offered to a second worker, and hides the task from the queue's answer path", async () => {
  const name = `claim_exclusive_${crypto.randomUUID()}`;
  await define(name);
  await startInstance(name);

  const [job] = await claimWhenReady("worker-1", name);
  expect(await claim("worker-2", name), "a live claim must not be handed out twice").toEqual([]);

  // The instance still reports who holds the claim — an operator must be able to see work in
  // progress — and the two-part token derived from the row cannot answer over a live claim.
  const listed = (await parkedInProcess(name, client))[0];
  expect(listed?.claimed_by).toBe("worker-1");
  expect(listed?.claim_expires).toBeTruthy();

  const { error } = await client.POST("/external-tasks/resolve", {
    body: { token: listed.token, result: { priced: 1 } },
  });
  expect(error, "an unclaimed handle must not answer over a live claim").toBeTruthy();
  expect(JSON.stringify(error)).toContain("worker-1");

  // The holder still can.
  const { error: ok } = await client.POST("/external-tasks/resolve", {
    body: { token: job.token, result: { priced: 2 } },
  });
  expect(ok, `the holder was refused: ${JSON.stringify(ok)}`).toBeUndefined();
});

test("release hands the task back at once and voids the releasing worker's token", async () => {
  const name = `claim_release_${crypto.randomUUID()}`;
  await define(name);
  await startInstance(name);

  const [first] = await claimWhenReady("worker-1", name);
  const { error } = await client.POST("/external-tasks/release", { body: { token: first.token } });
  expect(error, `release failed: ${JSON.stringify(error)}`).toBeUndefined();

  const [second] = await claimWhenReady("worker-2", name);
  expect(second.token).not.toBe(first.token);

  // Unlike an expiry, a release is deliberate — so the releasing worker's handle stops working
  // immediately rather than staying valid until someone else claims.
  const { error: stale } = await client.POST("/external-tasks/resolve", {
    body: { token: first.token, result: { priced: 1 } },
  });
  expect(stale, "a released token must not answer").toBeTruthy();

  const { error: ok } = await client.POST("/external-tasks/resolve", {
    body: { token: second.token, result: { priced: 3 } },
  });
  expect(ok, `the new holder was refused: ${JSON.stringify(ok)}`).toBeUndefined();
});

test("renew answers per token, not with a count", async () => {
  const name = `claim_renew_${crypto.randomUUID()}`;
  await define(name);
  await startInstance(name);
  const [job] = await claimWhenReady("worker-1", name, { lease_ms: 30_000 });

  const { data, error } = await client.POST("/external-tasks/renew", {
    body: { worker_id: "worker-1", tokens: [job.token], lease_ms: 60_000 },
  });
  expect(error, `renew failed: ${JSON.stringify(error)}`).toBeUndefined();
  // The token, not a number: a worker holding several claims has to know WHICH one it
  // still holds to abandon the right job. specs/external-task-queue.md.
  expect((data as any)?.renewed).toEqual([job.token]);
  expect((data as any)?.lost).toEqual([]);
  expect((data as any)?.cancelled).toEqual([]);
  // The interval is a value the worker reads rather than one it guesses.
  expect((data as any)?.renew_before_ms).toBeGreaterThan(0);

  // Scoped to the holder: a stranger renews nothing rather than stealing the lease, and is
  // told so in the terms it asked -- the token it named comes back as lost.
  const { data: other } = await client.POST("/external-tasks/renew", {
    body: { worker_id: "worker-2", tokens: [job.token], lease_ms: 60_000 },
  });
  expect((other as any)?.renewed).toEqual([]);
  expect((other as any)?.lost).toEqual([job.token]);

  // A renewal extends the grant, so the token it was granted under still answers.
  const { error: ok } = await client.POST("/external-tasks/resolve", {
    body: { token: job.token, result: { priced: 4 } },
  });
  expect(ok, `renewing invalidated the holder's own token: ${JSON.stringify(ok)}`).toBeUndefined();
});

test("a claim holder can answer on the error channel", async () => {
  const name = `claim_fail_${crypto.randomUUID()}`;
  await define(name);
  const id = await startInstance(name);
  const [job] = await claimWhenReady("worker-1", name);

  const { error } = await client.POST("/external-tasks/resolve", {
    body: { token: job.token, error: { code: "worker_failed", message: "the job died" } },
  });
  expect(error, `the error channel refused a claim token: ${JSON.stringify(error)}`).toBeUndefined();
  expect(await waitForInstance(id)).toBe("completed");
  expect((await outputsOf(id)).failed).toEqual({ route: "failed" });
});

test("claim filters by task, and takes a batch", async () => {
  const name = `claim_filter_${crypto.randomUUID()}`;
  await define(name);
  const ids = [await startInstance(name), await startInstance(name), await startInstance(name)];
  expect(ids.length).toBe(3);

  // The wrong task id matches nothing; the right one takes the batch, oldest park first.
  expect(await claim("worker-1", name, { task: "failed", limit: 10 })).toEqual([]);
  const got = await claimWhenReady("worker-1", name, { task: "work", limit: 10 });
  expect(got.length).toBeGreaterThan(0);
  for (const job of got) expect(job.task).toBe("work");
});

test("claim rejects a missing worker_id, and renew rejects a non-claim token", async () => {
  const name = `claim_badreq_${crypto.randomUUID()}`;
  await define(name);
  await startInstance(name);
  const [job] = await claimWhenReady("worker-1", name);

  const { error: noWorker } = await client.POST("/external-tasks/claim", {
    body: { worker_id: "", process: name } as never,
  });
  expect(noWorker, "worker_id is the claim's holder and cannot be blank").toBeTruthy();

  // The two-part token names no grant, so there is nothing for renew to extend.
  const twoPart = job.token.split(".").slice(0, 2).join(".");
  const { error: badToken } = await client.POST("/external-tasks/renew", {
    body: { worker_id: "worker-1", tokens: [twoPart] },
  });
  expect(badToken, "renew must refuse a token that names no claim").toBeTruthy();
});

// A lapsed claim on an only_once task may already have taken effect, so it is never handed out
// again, and the instance must learn why. specs/external-task-queue.md §external.lost.

async function defineOnlyOnce(name: string, extra: Record<string, unknown> = {}) {
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "work",
          action: { type: "external" as const, input: { job: "charge" }, result_schema: {} },
          only_once: true,
          output: "$: self.result",
          ...extra,
          switch: [{ goto: "end" }],
        },
        { id: "checked", output: { route: "checked", code: "$: last_error.code" }, switch: [{ goto: "end" }] },
      ] as never,
    },
  });
  if (error) throw new Error(`put definition failed: ${JSON.stringify(error)}`);
}

test("a lapsed claim on an only_once task is never handed out again, and raises external.lost", async () => {
  const name = `claim_lost_${crypto.randomUUID()}`;
  await defineOnlyOnce(name, { on_error: [{ code: ["external.lost"], goto: "$checked" }] });
  const id = await startInstance(name);

  // A short lease, then let it lapse without answering. Real time rather than /tick: this
  // suite's server is poll-driven, and /tick is only served with --poll 0.
  const [first] = await claimWhenReady("worker-1", name, { lease_ms: 300 });
  expect(first.token.split(".").length).toBe(3);
  await new Promise((r) => setTimeout(r, 500));

  // The second worker must get NOTHING: worker-1 may already have charged the card.
  const deadline = Date.now() + 20_000;
  let handedOut = true;
  while (Date.now() < deadline) {
    const got = await claim("worker-2", name);
    if (got.length === 0) {
      handedOut = false;
      break;
    }
    await new Promise((r) => setTimeout(r, 50));
  }
  expect(handedOut, "an only_once task whose holder lapsed was handed to a second worker").toBe(false);

  // And it does not just sit there: the engine reports why, catchably.
  expect(await waitForInstance(id, 20_000)).toBe("completed");
  expect((await outputsOf(id)).checked).toEqual({ route: "checked", code: "external.lost" });
});

test("external.lost is unknowable — an only_once task cannot buy a retry with not_reached", async () => {
  const name = `claim_lost_retry_${crypto.randomUUID()}`;
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "work",
          action: { type: "external" as const, input: {}, result_schema: {} },
          only_once: true,
          // Nothing came back, so nothing can be asserted about what happened: not_reached is
          // a claim about an error that RETURNED, and this one did not.
          on_error: [{ code: ["external.lost"], not_reached: true, retry: { retries: 2 }, goto: "$checked" }],
          switch: [{ goto: "end" }],
        },
        { id: "checked", switch: [{ goto: "end" }] },
      ] as never,
    },
  });
  expect(error, "not_reached must be refused for external.lost").toBeTruthy();
  expect(JSON.stringify(error)).toContain("external.lost");
});

test("a lapsed claim on an ordinary task just returns to the queue", async () => {
  const name = `claim_lapse_retryable_${crypto.randomUUID()}`;
  await define(name); // not only_once
  const id = await startInstance(name);

  const [first] = await claimWhenReady("worker-1", name, { lease_ms: 300 });
  await new Promise((r) => setTimeout(r, 500));

  // At-least-once is what a queue is for: without only_once, re-running is the right default.
  const [second] = await claimWhenReady("worker-2", name);
  expect(second.token).not.toBe(first.token);
  const { error } = await client.POST("/external-tasks/resolve", {
    body: { token: second.token, result: { priced: 7 } },
  });
  expect(error, `the second holder was refused: ${JSON.stringify(error)}`).toBeUndefined();
  expect(await waitForInstance(id)).toBe("completed");
  expect((await outputsOf(id)).work).toEqual({ priced: 7 });
});

test("a lost-claim row does not strand the rest of the batch, and filters isolate processes", async () => {
  const lostName = `batch_lost_${crypto.randomUUID()}`;
  const okName = `batch_ok_${crypto.randomUUID()}`;
  // Shared by both processes and nothing else on the server, so the claim below spans both while
  // naming no process. Unfiltered, it took other files' parked tasks and failed their resolves.
  const task = `batch_${crypto.randomUUID().replaceAll("-", "_")}`;
  await defineOnlyOnce(lostName, { id: task, on_error: [{ code: ["external.lost"], goto: "$checked" }] });
  await define(okName, workThenFailed(task));
  const lostId = await startInstance(lostName);
  const okId = await startInstance(okName);

  // Let the only_once task's holder lapse, so the next claim has to mark it lost.
  await claimWhenReady("worker-1", lostName, { lease_ms: 300 });
  await new Promise((r) => setTimeout(r, 500));

  // The lapsed row is skipped, not fatal: failing the request would leave the other task's grant
  // written and never handed to anyone.
  const deadline = Date.now() + 20_000;
  let got: any[] = [];
  while (Date.now() < deadline) {
    const { data, error } = await client.POST("/external-tasks/claim", {
      body: { worker_id: "worker-2", task, limit: 10 } as never,
    });
    expect(error, `a batch containing a lapsed only_once row failed: ${JSON.stringify(error)}`).toBeUndefined();
    got = ((data as any)?.items ?? []) as any[];
    if (got.length) break;
    await new Promise((r) => setTimeout(r, 50));
  }
  expect(got.map((j) => j.process), "only the claimable task in the batch is handed out").toEqual([okName]);

  const { error } = await client.POST("/external-tasks/resolve", {
    body: { token: got[0].token, result: { priced: 11 } },
  });
  expect(error, `the batch's good row could not be answered: ${JSON.stringify(error)}`).toBeUndefined();
  expect(await waitForInstance(okId)).toBe("completed");

  // And the lapsed one still reported itself rather than going quiet.
  expect(await waitForInstance(lostId, 20_000)).toBe("completed");
  expect((await outputsOf(lostId)).checked?.code).toBe("external.lost");
});

test("a claim carries its deadline relative to now, so a worker budgets without the server's clock", async () => {
  const name = `claim_deadline_${crypto.randomUUID()}`;
  const tasks = workThenFailed();
  (tasks[0] as any).action.timeout = "10s";
  await define(name, tasks);
  await startInstance(name);

  const [job] = await claimWhenReady("worker-1", name);
  expect(job.deadline_in_ms, "the task has a timeout, so the claim must say how much of it is left").toBeGreaterThan(0);
  expect(job.deadline_in_ms).toBeLessThanOrEqual(10_000);

  const plain = `claim_no_deadline_${crypto.randomUUID()}`;
  await define(plain);
  await startInstance(plain);
  const [none] = await claimWhenReady("worker-1", plain);
  expect(none.deadline_in_ms, "absent means the task waits forever; 0 would read as already due").toBeUndefined();
});

test("claim filters by process — one worker fleet does not take another's work", async () => {
  const mine = `filter_mine_${crypto.randomUUID()}`;
  const theirs = `filter_theirs_${crypto.randomUUID()}`;
  await define(mine);
  await define(theirs);
  await startInstance(mine);
  const theirsId = await startInstance(theirs);

  const got = await claimWhenReady("worker-1", mine, { limit: 10 });
  for (const job of got) {
    expect(job.process, "a process filter must not hand out another process's work").toBe(mine);
  }

  // Theirs is untouched and still claimable by its own fleet.
  const other = await claimWhenReady("worker-2", theirs);
  expect(other.length).toBe(1);
  await client.POST("/external-tasks/resolve", { body: { token: other[0].token, result: { priced: 1 } } });
  expect(await waitForInstance(theirsId)).toBe("completed");
});

// Two external tasks in a row: the second is a new occurrence, so the first's claim must not carry over.
function twoInARow() {
  const ext = (id: string, next: string) => ({
    id,
    action: { type: "external" as const, input: { step: id }, result_schema: {} },
    output: "$: self.result",
    switch: [{ goto: next }],
  });
  return [ext("first", "$second"), ext("second", "end")];
}

async function parkedOn(id: string, task: string) {
  const deadline = Date.now() + 10_000;
  while (Date.now() < deadline) {
    const t = await parkedTask(id);
    if (t?.task === task) return t;
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error(`instance ${id} never parked on ${task}`);
}

test("a claimed answer leaves no claim on the next external task", async () => {
  const name = `claim_next_${crypto.randomUUID()}`;
  await define(name, twoInARow());
  const id = await startInstance(name);

  const [job] = await claimWhenReady("worker-1", name, { task: "first" });
  await client.POST("/external-tasks/resolve", { body: { token: job.token, result: { ok: 1 } } });

  const next = await parkedOn(id, "second");
  expect(next.claimed_by, "the second task inherited the first task's claim").toBeUndefined();
  const { error } = await client.POST("/external-tasks/resolve", {
    body: { token: next.token, result: { ok: 2 } },
  });
  expect(error, `an unclaimed handle was refused on the next task: ${JSON.stringify(error)}`).toBeUndefined();
  expect(await waitForInstance(id)).toBe("completed");
});

test("the next external task is claimable at once after a claimed answer", async () => {
  const name = `claim_next_now_${crypto.randomUUID()}`;
  await define(name, twoInARow());
  const id = await startInstance(name);

  const [job] = await claimWhenReady("worker-1", name, { task: "first" });
  await client.POST("/external-tasks/resolve", { body: { token: job.token, result: { ok: 1 } } });
  await parkedOn(id, "second");

  const got = await claim("worker-2", name, { task: "second" });
  expect(got.length, "the second task waited for the first task's lease to lapse").toBe(1);
});

test("renewing a finished task's token reports it lost, though the same worker holds the next", async () => {
  const name = `claim_renew_stale_${crypto.randomUUID()}`;
  await define(name, twoInARow());
  const id = await startInstance(name);

  const [first] = await claimWhenReady("worker-1", name, { task: "first" });
  await client.POST("/external-tasks/resolve", { body: { token: first.token, result: { ok: 1 } } });
  await parkedOn(id, "second");
  const [second] = await claimWhenReady("worker-1", name, { task: "second" });

  const { data } = await client.POST("/external-tasks/renew", {
    body: { worker_id: "worker-1", tokens: [first.token, second.token] },
  });
  expect((data as any)?.lost, "the first task's arming is over; its token must not renew the second's claim")
    .toEqual([first.token]);
  expect((data as any)?.renewed).toEqual([second.token]);
});
