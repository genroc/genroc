import { expect, test } from "vitest";
import { childrenOfTask, client, waitForInstance } from "../helpers/client.ts";
import { waitForParked } from "../helpers/external.ts";

// The terminal stop. Cancel is deliberately not a mode of pause: pause exists to be
// reversible and a cancel is final, which is why it is a status beside `failed` and why
// retry refuses it. specs/pause-resume.md.

async function define(name: string, tasks?: unknown[]) {
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: (tasks ?? [
        {
          id: "work",
          action: { type: "external" as const, input: { job: "compute" }, result_schema: {} },
          output: "$: self.result",
          switch: [{ goto: "end" }],
        },
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

async function statusOf(id: string): Promise<string> {
  const { data, error } = await client.GET("/instances/{id}", { params: { path: { id } } });
  if (error) throw new Error(`get failed: ${JSON.stringify(error)}`);
  return (data as any).status;
}

async function cancel(id: string) {
  return client.POST("/instances/{id}/cancel", { params: { path: { id } } });
}

async function claimWhenReady(worker: string, process: string) {
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    const { data } = await client.POST("/external-tasks/claim", {
      body: { worker_id: worker, process, lease_ms: 30_000 } as never,
    });
    const items = ((data as any)?.items ?? []) as any[];
    if (items.length) return items;
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error(`nothing claimable for ${process} in time`);
}

test("cancel stops a parked instance for good", async () => {
  const name = `cancel_basic_${crypto.randomUUID()}`;
  await define(name);
  const id = await start(name);
  await waitForParked(id);

  const { error } = await cancel(id);
  expect(error, `cancel failed: ${JSON.stringify(error)}`).toBeUndefined();
  expect(await statusOf(id)).toBe("cancelled");

  // It stays stopped: nothing re-claims a terminal row, so the status is the same a moment
  // later rather than something a tick undoes.
  await new Promise((r) => setTimeout(r, 300));
  expect(await statusOf(id)).toBe("cancelled");
});

test("a cancelled instance is neither resumable nor retryable", async () => {
  const name = `cancel_final_${crypto.randomUUID()}`;
  await define(name);
  const id = await start(name);
  await waitForParked(id);
  await cancel(id);

  // Retry is refused outright: it revives a tree whose DEFINITION ran out of attempts, and an
  // operator's stop was never an attempt. This is the conflation migration 022 removed.
  const { error: retryErr } = await client.POST("/instances/{id}/retry", {
    params: { path: { id } },
  });
  expect(retryErr, "a cancelled instance must not be retryable").toBeDefined();

  // Resume refuses too, on the settled-and-never-will path -- which is only reachable
  // because `cancelled` is terminal. Its advice must not name retry, since retry is the
  // one verb that refuses a cancel outright.
  const { error: resumeErr } = await client.POST("/instances/{id}/resume", {
    params: { path: { id } },
  });
  expect(resumeErr, "a cancelled instance must not resume").toBeDefined();
  expect(JSON.stringify(resumeErr)).toContain("new instance");
  expect(await statusOf(id)).toBe("cancelled");
});

test("cancel disposes of a paused tree", async () => {
  const name = `cancel_paused_${crypto.randomUUID()}`;
  await define(name);
  const id = await start(name);
  await waitForParked(id);

  await client.POST("/instances/{id}/pause", { params: { path: { id } } });
  expect(await statusOf(id)).toBe("paused");

  // The reason cancel exists: a paused tree is live work that nothing else can dispose of.
  await cancel(id);
  expect(await statusOf(id)).toBe("cancelled");
});

test("cancel is an assertion: a settled instance reports rather than fails", async () => {
  const name = `cancel_settled_${crypto.randomUUID()}`;
  await define(name, [{ id: "done", output: { ok: true }, switch: [{ goto: "end" }] }]);
  const id = await start(name);
  await waitForInstance(id);

  // 204: nothing was live to stop. An error here would stop a group of ids from converging
  // when it is re-run. specs/id-list-commands.md.
  const { error } = await cancel(id);
  expect(error, `cancelling a settled tree must not fail: ${JSON.stringify(error)}`).toBeUndefined();
  expect(await statusOf(id)).toBe("completed");
});

test("a cancel reaches a worker holding the claim, through its own heartbeat", async () => {
  const name = `cancel_claimed_${crypto.randomUUID()}`;
  await define(name);
  const id = await start(name);
  const [job] = await claimWhenReady("worker-1", name);

  await cancel(id);

  // genroc cannot call a worker -- workers dial in -- so the renewal is the only channel that
  // reaches work in flight. The token comes back named, which is what lets a worker holding
  // several claims abandon this one alone.
  const { data } = await client.POST("/external-tasks/renew", {
    body: { worker_id: "worker-1", tokens: [job.token], lease_ms: 30_000 },
  });
  expect((data as any)?.cancelled).toEqual([job.token]);
  expect((data as any)?.renewed).toEqual([]);

  // And an answer sent anyway is refused, in terms that say to stop rather than to retry.
  const { error } = await client.POST("/external-tasks/resolve", {
    body: { token: job.token, result: { priced: 1 } },
  });
  expect(error, "a cancelled task must not accept an outcome").toBeDefined();
  expect(JSON.stringify(error)).toContain("cancel");

  // The release is how the row stops waiting out a lease nobody is serving.
  const { error: relErr } = await client.POST("/external-tasks/release", {
    body: { token: job.token },
  });
  expect(relErr, `releasing a cancelled claim failed: ${JSON.stringify(relErr)}`).toBeUndefined();
});

test("cancel takes the whole tree, and is refused on a descendant", async () => {
  const child = `cancel_child_${crypto.randomUUID()}`;
  await define(child);
  const parent = `cancel_parent_${crypto.randomUUID()}`;
  const { error: defErr } = await client.PUT("/definitions", {
    body: {
      name: parent,
      tasks: [
        {
          id: "spawn",
          action: { type: "child" as const, name: child, result_schema: {} },
          switch: [{ goto: "end" }],
        },
      ] as never,
    },
  });
  if (defErr) throw new Error(`put parent failed: ${JSON.stringify(defErr)}`);

  const id = await start(parent);
  // The child parks on its own external task, which is the point at which both rows are live.
  const kidID = await (async () => {
    const deadline = Date.now() + 20_000;
    while (Date.now() < deadline) {
      // A single `child` action yields the child's id directly, not a keyed map.
      const kid = (await childrenOfTask(id, "spawn")) as string | undefined;
      if (kid) return kid;
      await new Promise((r) => setTimeout(r, 50));
    }
    throw new Error("the parent never spawned a child");
  })();
  await waitForParked(kidID);

  // Root-only: a descendant id is refused in favour of its root, because the tree is the unit
  // that stops. A subtree cancel would strand the parent waiting on a child that never answers.
  const { error: kidErr } = await cancel(kidID);
  expect(kidErr, "cancelling a descendant directly must be refused").toBeDefined();

  await cancel(id);
  expect(await statusOf(id)).toBe("cancelled");
  expect(await statusOf(kidID)).toBe("cancelled");
});

// The "and nothing else" half of the guarantee, at the HTTP surface. Each verb is closed for
// its own reason and in its own package, so the rule holds only as long as every one of them
// keeps holding it -- and an endpoint added later is what this is here to catch. What is
// asserted is not the shape of each refusal (some error, some are assertions that report a
// no-op, and both are right) but the one thing they must share: the status does not move.
test("once a cancel lands, no operator verb moves the instance", async () => {
  const name = `cancel_closed_${crypto.randomUUID()}`;
  await define(name);
  const id = await start(name);
  await waitForParked(id);

  // A second version to aim an upgrade at, so that verb is genuinely attempted rather than
  // failing on a missing target.
  await define(name, [
    {
      id: "work",
      action: { type: "external" as const, input: { job: "v2" }, result_schema: {} },
      output: "$: self.result",
      switch: [{ goto: "end" }],
    },
  ]);

  await cancel(id);
  expect(await statusOf(id)).toBe("cancelled");

  const verbs: [string, () => Promise<unknown>][] = [
    ["pause", () => client.POST("/instances/{id}/pause", { params: { path: { id } } })],
    ["resume", () => client.POST("/instances/{id}/resume", { params: { path: { id } } })],
    ["retry", () => client.POST("/instances/{id}/retry", { params: { path: { id } } })],
    ["cancel again", () => cancel(id)],
    [
      "upgrade",
      () =>
        client.POST("/instances/{id}/upgrade", {
          params: { path: { id } },
          body: { to_version: 2 } as never,
        }),
    ],
    [
      "signal",
      () =>
        client.POST("/external-tasks/signal", {
          body: { instance_id: id, task_id: "work", result: { ok: true } } as never,
        }),
    ],
  ];

  for (const [verb, call] of verbs) {
    await call();
    expect(await statusOf(id), `${verb} moved a cancelled instance`).toBe("cancelled");
  }
});
