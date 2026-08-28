import { parkedInProcess } from "../helpers/external.ts";
import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// The pull half of the external-task queue: a worker claims parked work, holds it for a
// visibility timeout, renews or releases it, and answers under the token the claim granted.
// specs/external-task-queue.md.

async function define(name: string, tasks?: unknown[]) {
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: (tasks ?? [
        {
          id: "work",
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
      ]) as never,
    },
  });
  if (error) throw new Error(`put definition failed: ${JSON.stringify(error)}`);
}

async function start(name: string): Promise<string> {
  const { data, error } = await client.POST("/instances", { body: { process: name } });
  if (error) throw new Error(`start failed: ${JSON.stringify(error)}`);
  return data!.id;
}

async function claim(worker: string, process: string, opts: Record<string, unknown> = {}) {
  const { data, error } = await client.POST("/external-tasks/claim", {
    body: { worker_id: worker, process, ...opts } as never,
  });
  if (error) throw new Error(`claim failed: ${JSON.stringify(error)}`);
  return ((data as any)?.items ?? []) as any[];
}

// claimUntil polls until the task is parked and claimable — a fresh instance takes a tick to
// reach its external task.
async function claimWhenReady(worker: string, process: string, opts: Record<string, unknown> = {}) {
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    const got = await claim(worker, process, opts);
    if (got.length) return got;
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error(`nothing claimable for ${process} in time`);
}

async function outputsOf(id: string): Promise<any> {
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  return (data as any)?.state?.outputs ?? {};
}

test("a claim leases the task, and the granted token answers it", async () => {
  const name = `claim_basic_${crypto.randomUUID()}`;
  await define(name);
  const id = await start(name);

  const [job] = await claimWhenReady("worker-1", name);
  // The claim token is three-part: instance, arming, grant. The queue's own two-part token
  // names no grant and is refused while this one is live.
  expect(job.token.split(".").length).toBe(3);
  expect(job.task_id).toBe("work");
  expect(job.input).toEqual({ job: "compute" });
  expect(job.raises).toHaveProperty("worker_failed");

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
  await start(name);

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
  await start(name);

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

test("renew extends this worker's claims and reports what it still held", async () => {
  const name = `claim_renew_${crypto.randomUUID()}`;
  await define(name);
  await start(name);
  const [job] = await claimWhenReady("worker-1", name, { lease_ms: 30_000 });

  const { data, error } = await client.POST("/external-tasks/renew", {
    body: { worker_id: "worker-1", tokens: [job.token], lease_ms: 60_000 },
  });
  expect(error, `renew failed: ${JSON.stringify(error)}`).toBeUndefined();
  expect((data as any)?.renewed).toBe(1);

  // Scoped to the holder: a stranger renews nothing rather than stealing the lease.
  const { data: other } = await client.POST("/external-tasks/renew", {
    body: { worker_id: "worker-2", tokens: [job.token], lease_ms: 60_000 },
  });
  expect((other as any)?.renewed).toBe(0);

  // A renewal extends the grant, so the token it was granted under still answers.
  const { error: ok } = await client.POST("/external-tasks/resolve", {
    body: { token: job.token, result: { priced: 4 } },
  });
  expect(ok, `renewing invalidated the holder's own token: ${JSON.stringify(ok)}`).toBeUndefined();
});

test("a claim holder can answer on the error channel", async () => {
  const name = `claim_fail_${crypto.randomUUID()}`;
  await define(name);
  const id = await start(name);
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
  const ids = [await start(name), await start(name), await start(name)];
  expect(ids.length).toBe(3);

  // The wrong task id matches nothing; the right one takes the batch, oldest park first.
  expect(await claim("worker-1", name, { task: "failed", limit: 10 })).toEqual([]);
  const got = await claimWhenReady("worker-1", name, { task: "work", limit: 10 });
  expect(got.length).toBeGreaterThan(0);
  for (const job of got) expect(job.task_id).toBe("work");
});

test("claim rejects a missing worker_id, and renew rejects a non-claim token", async () => {
  const name = `claim_badreq_${crypto.randomUUID()}`;
  await define(name);
  await start(name);
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

// Phase 3: a claim that lapses on an only_once task. The work may already have taken effect,
// so it must never be handed out again — and the instance must learn why rather than sitting
// unclaimable. specs/external-task-queue.md §external.lost.

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
        { id: "checked", output: { route: "checked", code: "$: error.code" }, switch: [{ goto: "end" }] },
      ] as never,
    },
  });
  if (error) throw new Error(`put definition failed: ${JSON.stringify(error)}`);
}

test("a lapsed claim on an only_once task is never handed out again, and raises external.lost", async () => {
  const name = `claim_lost_${crypto.randomUUID()}`;
  await defineOnlyOnce(name, { on_error: [{ code: ["external.lost"], goto: "$checked" }] });
  const id = await start(name);

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
          on_error: [{ code: ["external.lost"], not_reached: true, retry: { attempts: 2 }, goto: "$checked" }],
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
  const id = await start(name);

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
  await defineOnlyOnce(lostName, { on_error: [{ code: ["external.lost"], goto: "$checked" }] });
  await define(okName);
  const lostId = await start(lostName);
  const okId = await start(okName);

  // Let the only_once task's holder lapse, so the next claim has to mark it lost.
  await claimWhenReady("worker-1", lostName, { lease_ms: 300 });
  await new Promise((r) => setTimeout(r, 500));

  // A claim naming no process spans both. The lapsed row is skipped, not fatal: failing the
  // request would leave the other task's grant written and never handed to anyone.
  const deadline = Date.now() + 20_000;
  let got: any[] = [];
  while (Date.now() < deadline) {
    const { data, error } = await client.POST("/external-tasks/claim", {
      body: { worker_id: "worker-2", limit: 10 } as never,
    });
    expect(error, `a batch containing a lapsed only_once row failed: ${JSON.stringify(error)}`).toBeUndefined();
    got = ((data as any)?.items ?? []).filter((j: any) => j.process === okName);
    if (got.length) break;
    await new Promise((r) => setTimeout(r, 50));
  }
  expect(got.length, "the claimable task in the batch was never handed out").toBe(1);

  const { error } = await client.POST("/external-tasks/resolve", {
    body: { token: got[0].token, result: { priced: 11 } },
  });
  expect(error, `the batch's good row could not be answered: ${JSON.stringify(error)}`).toBeUndefined();
  expect(await waitForInstance(okId)).toBe("completed");

  // And the lapsed one still reported itself rather than going quiet.
  expect(await waitForInstance(lostId, 20_000)).toBe("completed");
  expect((await outputsOf(lostId)).checked?.code).toBe("external.lost");
});

test("claim filters by process — one worker fleet does not take another's work", async () => {
  const mine = `filter_mine_${crypto.randomUUID()}`;
  const theirs = `filter_theirs_${crypto.randomUUID()}`;
  await define(mine);
  await define(theirs);
  await start(mine);
  const theirsId = await start(theirs);

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
