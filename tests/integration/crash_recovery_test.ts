import { expect, test, beforeAll, afterAll } from "vitest";
import { join } from "path";
import { tmpdir } from "os";
import { spawnSync } from "child_process";
import { buildGenrocBinary, startGenroc } from "../helpers/server.ts";
import { startMockService, waitForInstance } from "../helpers/client.ts";

// The sqlite and postgres vitest projects run this file in parallel, and both read
// the global POSTGRES_DSN, so offset the (otherwise fixed) genroc ports per project
// to keep their own genroc1/genroc2 processes from colliding.
const PORT_OFFSET = (Number(process.env.GENROC_PORT ?? 8888) - 8888) * 4;
const GENROC1_PORT = 20011 + PORT_OFFSET;
const GENROC2_PORT = 20012 + PORT_OFFSET;
// Second pair, for the pause-crash tests below (they run in the same file but must
// not reuse the ports above while those servers are still shutting down).
const PAUSE1_PORT = 20061 + PORT_OFFSET;
const PAUSE2_PORT = 20062 + PORT_OFFSET;
// Third and fourth pairs, for the only_once.interrupted recovery tests at the end of
// the file — same reason as above: never reuse a pair still shutting down.
const RECOVER1_PORT = 20071 + PORT_OFFSET;
const RECOVER2_PORT = 20072 + PORT_OFFSET;
const RERUN1_PORT = 20081 + PORT_OFFSET;
const RERUN2_PORT = 20082 + PORT_OFFSET;

let genrocBin: string;
let crashPgDSN: string | undefined;
let tempDbName: string | undefined;

function replaceDbName(dsn: string, dbName: string): string {
  const url = new URL(dsn);
  url.pathname = `/${dbName}`;
  return url.toString();
}

beforeAll(async () => {
  genrocBin = await buildGenrocBinary();

  const rawDsn = process.env.POSTGRES_DSN;
  if (rawDsn) {
    tempDbName = `genroc_crash_${Date.now()}`;
    const adminDsn = replaceDbName(rawDsn, "postgres");
    const result = spawnSync(
      "psql",
      [adminDsn, "-c", `CREATE DATABASE ${tempDbName}`],
      {
        stdio: "pipe",
      },
    );
    if (result.status !== 0) {
      throw new Error(
        `Failed to create crash recovery database: ${result.stderr.toString()}`,
      );
    }
    crashPgDSN = replaceDbName(rawDsn, tempDbName);
  }
}, 120_000);

afterAll(() => {
  if (tempDbName) {
    const adminDsn = replaceDbName(process.env.POSTGRES_DSN!, "postgres");
    spawnSync(
      "psql",
      [adminDsn, "-c", `DROP DATABASE ${tempDbName} WITH (FORCE)`],
      { stdio: "pipe" },
    );
  }
});

test("crash recovery — new worker re-executes an unconfirmed task after the previous worker crashes", async () => {
  const db = crashPgDSN ? "" : join(tmpdir(), `genroc_crash_${Date.now()}.db`);

  // firstRequestDelayMs: Infinity keeps the connection open so the task
  // stays in-flight when we crash the worker.
  const mock = await startMockService(0, {
    response: { done: true },
    firstRequestDelayMs: Infinity,
  });

  const genroc1 = await startGenroc(genrocBin, GENROC1_PORT, db, crashPgDSN);
  try {
    const processName = `crash_recovery_${crypto.randomUUID()}`;
    await genroc1.client.PUT("/definitions", {
      body: {
        name: processName,

        tasks: [
          {
            id: "work",
            action: {
              type: "fetch" as const,
              url: `http://localhost:${mock.port}/action`,
            },
            // Long enough that the task never times out before the crash.
            timeout: 120_000,
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: startData } = await genroc1.client.POST("/instances", {
      body: { process: processName },
    });
    const instanceId = startData!.id;

    // Wait until genroc1 has claimed the instance and the task is in-flight.
    await Promise.race([
      mock.firstRequestReceived,
      new Promise<never>((_, reject) =>
        setTimeout(
          () => reject(new Error("mock never received first request")),
          15_000,
        ),
      ),
    ]);

    // Crash: SIGKILL leaves the lease in the database without releasing it.
    genroc1.crash();

    // Manual-tick mode (--poll 0): /tick is only available when the continuous
    // pump is off, and it lets us drive reclaim deterministically.
    const genroc2 = await startGenroc(genrocBin, GENROC2_PORT, db, crashPgDSN, 0);
    // The engine lease is 10 s. Instead of waiting it out, shift genroc2's
    // clock forward so genroc1's lease is already expired from its view,
    // and tick immediately so it reclaims the instance.
    await genroc2.client.POST("/tick", { body: { advance_ms: 12_000 } });
    try {
      const finalStatus = await waitForInstance(
        instanceId,
        15_000,
        genroc2.client,
      );

      // genroc2 must have re-executed the task and completed the instance.
      expect(finalStatus).toBe("completed");
      // Once by genroc1 (abandoned at crash), once by genroc2 (confirmed).
      expect(mock.requestCount()).toBe(2);
    } finally {
      genroc2.stop();
    }
  } finally {
    genroc1.crash(); // no-op if already dead
    await mock.stop();
  }
}, 60_000);

test("crash recovery — an only_once task is failed (not re-executed) after a lease takeover", async () => {
  const db = crashPgDSN ? "" : join(tmpdir(), `genroc_crash_once_${Date.now()}.db`);

  // The first request hangs so the task is in-flight when we crash the worker.
  const mock = await startMockService(0, {
    response: { done: true },
    firstRequestDelayMs: Infinity,
  });

  const genroc1 = await startGenroc(genrocBin, GENROC1_PORT, db, crashPgDSN);
  try {
    const processName = `crash_only_once_${crypto.randomUUID()}`;
    await genroc1.client.PUT("/definitions", {
      body: {
        name: processName,
        tasks: [
          {
            id: "work",
            action: {
              type: "fetch" as const,
              url: `http://localhost:${mock.port}/action`,
            },
            // only_once: the engine must not re-run this on a lease takeover, since
            // the call may already have happened on the crashed worker.
            only_once: true,
            timeout: 120_000,
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: startData } = await genroc1.client.POST("/instances", {
      body: { process: processName },
    });
    const instanceId = startData!.id;

    // Wait until genroc1 has claimed the instance and the task is in-flight.
    await Promise.race([
      mock.firstRequestReceived,
      new Promise<never>((_, reject) =>
        setTimeout(
          () => reject(new Error("mock never received first request")),
          15_000,
        ),
      ),
    ]);

    genroc1.crash();

    const genroc2 = await startGenroc(genrocBin, GENROC2_PORT, db, crashPgDSN, 0);
    await genroc2.client.POST("/tick", { body: { advance_ms: 12_000 } });
    try {
      const finalStatus = await waitForInstance(
        instanceId,
        15_000,
        genroc2.client,
      );

      // genroc2 detected the takeover and refused to re-execute the only_once task.
      expect(finalStatus).toBe("failed");
      const { data } = await genroc2.client.GET("/instances/{id}/detail", {
        params: { path: { id: instanceId } },
      });
      expect(data!.error_message).toContain("only_once");
      // Only genroc1's abandoned attempt — genroc2 never sent the request.
      expect(mock.requestCount()).toBe(1);
    } finally {
      genroc2.stop();
    }
  } finally {
    genroc1.crash();
    await mock.stop();
  }
}, 60_000);

// The one path that only a crash can reach.
//
// A pause normally lands in SQL: the worker holding the instance writes its finished
// task and the CASE in UpdateInstance turns 'pausing' into 'paused'. If that worker
// dies first, the row is stranded leased-but-dead in 'pausing', and only a reclaiming
// worker can settle it — which is the entire reason 'pausing' stays in the claim
// predicate. This drives that deterministically: hold the task open, pause, SIGKILL,
// then let a second worker reclaim.
async function pauseThenCrash(
  processName: string,
  db: string,
  mockPort: number,
  onlyOnce: boolean,
  // Optional on_error rules on the held task, plus the tasks they route to.
  opts: { onError?: unknown[]; extraTasks?: unknown[] } = {},
) {
  const genroc1 = await startGenroc(genrocBin, PAUSE1_PORT, db, crashPgDSN);
  const { error } = await genroc1.client.PUT("/definitions", {
    body: {
      name: processName,
      tasks: [
        {
          id: "work",
          ...(onlyOnce ? { only_once: true } : {}),
          ...(opts.onError ? { on_error: opts.onError } : {}),
          action: {
            type: "fetch" as const,
            url: `http://localhost:${mockPort}/action`,
          },
          timeout: 120_000,
          switch: [{ goto: "end" }],
        },
        ...(opts.extraTasks ?? []),
      ],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(error).toBeUndefined();
  const { data: startData } = await genroc1.client.POST("/instances", {
    body: { process: processName },
  });
  return { genroc1, instanceId: startData!.id };
}

test("a pausing instance whose worker crashes is settled to paused by the reclaimer", async () => {
  const db = crashPgDSN ? "" : join(tmpdir(), `genroc_pause_crash_${Date.now()}.db`);
  const mock = await startMockService(0, {
    response: { done: true },
    firstRequestDelayMs: Infinity,
  });
  const name = `pause_crash_${crypto.randomUUID()}`;
  const { genroc1, instanceId } = await pauseThenCrash(name, db, mock.port, false);

  try {
    // Wait until the task is genuinely in flight, so the row is leased.
    await mock.firstRequestReceived;

    // Leased, so the pause can only be recorded as a request: the worker is
    // mid-task and cannot be stopped.
    await genroc1.client.POST("/instances/{id}/pause", {
      params: { path: { id: instanceId } },
    });
    const { data: mid } = await genroc1.client.GET("/instances/{id}/detail", {
      params: { path: { id: instanceId } },
    });
    expect(mid!.status).toBe("pausing");

    // Crash before the worker can land the pause.
    genroc1.crash();

    const genroc2 = await startGenroc(genrocBin, PAUSE2_PORT, db, crashPgDSN, 0);
    try {
      // Expire the dead lease from genroc2's view and let it reclaim.
      await genroc2.client.POST("/tick", { body: { advance_ms: 12_000 } });

      const { data: after } = await genroc2.client.GET("/instances/{id}/detail", {
        params: { path: { id: instanceId } },
      });
      // Settled, not advanced: the pause the operator asked for is honoured, and
      // the abandoned task is NOT re-executed on the way.
      expect(after!.status).toBe("paused");
      expect(mock.requestCount()).toBe(1);

      // This is the one case where the deferred landing is audited, because it
      // went through the engine rather than a worker's own write. Audit rows are
      // buffered and flushed on a 5ms ticker, so poll rather than read once.
      let events: string[] = [];
      for (let i = 0; i < 40 && !events.includes("inst_paused"); i++) {
        const { data: logs } = await genroc2.client.GET("/instances/{id}/logs", {
          params: { path: { id: instanceId }, query: { limit: 100 } },
        });
        events = (logs!.items ?? []).map((l) => l.event as string);
        if (!events.includes("inst_paused")) await new Promise((r) => setTimeout(r, 50));
      }
      expect(events).toContain("inst_paused");

      // And it resumes normally from there — the task runs on the next tick.
      await genroc2.client.POST("/instances/{id}/resume", {
        params: { path: { id: instanceId } },
      });
      await genroc2.client.POST("/tick", {});
      expect(mock.requestCount()).toBe(2);
    } finally {
      genroc2.stop();
    }
  } finally {
    genroc1.crash(); // no-op if already dead
    await mock.stop();
  }
}, 60_000);

// The pausing counterpart of the routing tests at the end of this file, and the one case
// where the two halves of the rule are visible separately: the interruption is decided
// immediately (its evidence does not survive the write that settles a pause), while the
// handler itself runs only when the operator resumes.
//
// Asserted twice over, because the two say different things: `task` reports where the
// instance was parked, and resuming proves that position is the one that runs — if it had
// parked on the interrupted task instead, the resume would re-run the charge.
test("a pausing only_once instance with a handler pauses at the handler and runs it on resume", async () => {
  const db = crashPgDSN ? "" : join(tmpdir(), `genroc_pause_route_${Date.now()}.db`);
  const charge = await startMockService(0, {
    response: { done: true },
    firstRequestDelayMs: Infinity,
  });
  const verify = await startMockService(0, { response: { exists: true } });

  const name = `pause_route_${crypto.randomUUID()}`;
  const { genroc1, instanceId } = await pauseThenCrash(name, db, charge.port, true, {
    onError: [{ code: ["only_once.interrupted"], goto: "$verify" }],
    extraTasks: [
      {
        id: "verify",
        action: {
          type: "fetch" as const,
          url: `http://localhost:${verify.port}/charges`,
        },
        switch: [{ goto: "end" }],
      },
    ],
  });

  try {
    await charge.firstRequestReceived;
    await genroc1.client.POST("/instances/{id}/pause", {
      params: { path: { id: instanceId } },
    });
    genroc1.crash();

    const genroc2 = await startGenroc(genrocBin, PAUSE2_PORT, db, crashPgDSN, 0);
    try {
      await genroc2.client.POST("/tick", { body: { advance_ms: 12_000 } });

      // Decided, then suspended: the routing happened, and the pause the operator
      // asked for still landed on the write that carried it.
      const { data: after } = await genroc2.client.GET("/instances/{id}/detail", {
        params: { path: { id: instanceId } },
      });
      expect(after!.status).toBe("paused");
      expect(after!.task).toBe("verify"); // parked at the handler, not the charge
      expect(charge.requestCount()).toBe(1);
      expect(verify.requestCount()).toBe(0); // the handler has NOT run yet

      // Resuming runs the handler — not the interrupted charge, which is what
      // parking on the right task means.
      await genroc2.client.POST("/instances/{id}/resume", {
        params: { path: { id: instanceId } },
      });
      await genroc2.client.POST("/tick", {});
      const finalStatus = await waitForInstance(instanceId, 15_000, genroc2.client);
      expect(finalStatus).toBe("completed");
      expect(verify.requestCount()).toBe(1);
      expect(charge.requestCount()).toBe(1);
    } finally {
      genroc2.stop();
    }
  } finally {
    genroc1.crash();
    await charge.stop();
    await verify.stop();
  }
}, 60_000);

test("a pausing only_once instance whose worker crashes fails instead of pausing", async () => {
  const db = crashPgDSN ? "" : join(tmpdir(), `genroc_pause_once_${Date.now()}.db`);
  const mock = await startMockService(0, {
    response: { done: true },
    firstRequestDelayMs: Infinity,
  });
  const name = `pause_once_${crypto.randomUUID()}`;
  const { genroc1, instanceId } = await pauseThenCrash(name, db, mock.port, true);

  try {
    await mock.firstRequestReceived;
    await genroc1.client.POST("/instances/{id}/pause", {
      params: { path: { id: instanceId } },
    });
    genroc1.crash();

    const genroc2 = await startGenroc(genrocBin, PAUSE2_PORT, db, crashPgDSN, 0);
    try {
      await genroc2.client.POST("/tick", { body: { advance_ms: 12_000 } });

      // The interrupted call may already have happened on the dead worker, so
      // pausing here would launder an at-most-once violation into a silent
      // re-execution on resume. The instance fails instead — and stays failed,
      // because a failure is an outcome and a pause is not.
      const { data: after } = await genroc2.client.GET("/instances/{id}/detail", {
        params: { path: { id: instanceId } },
      });
      expect(after!.status).toBe("failed");
      expect(after!.error_message).toContain("only_once");
      expect(mock.requestCount()).toBe(1);
    } finally {
      genroc2.stop();
    }
  } finally {
    genroc1.crash();
    await mock.stop();
  }
}, 60_000);

// An interrupted only_once task is not simply refused: the engine hands the situation to
// the definition as only_once.interrupted, which on_error can catch. The point of catching
// it is that the definition can ask the system of record what actually happened —
// something the engine can never know — and then carry on.
//
// This is the same crash as the test above, with a handler added. The action endpoint must
// still be hit exactly once (genroc1's abandoned attempt); the verify endpoint is what the
// recovery costs.
test("crash recovery — an interrupted only_once task routes to its on_error handler", async () => {
  const db = crashPgDSN ? "" : join(tmpdir(), `genroc_crash_route_${Date.now()}.db`);

  const charge = await startMockService(0, {
    response: { done: true },
    firstRequestDelayMs: Infinity, // hangs, so the task is in-flight when we crash
  });
  const verify = await startMockService(0, { response: { exists: true } });

  const genroc1 = await startGenroc(genrocBin, RECOVER1_PORT, db, crashPgDSN);
  try {
    const processName = `crash_route_${crypto.randomUUID()}`;
    await genroc1.client.PUT("/definitions", {
      body: {
        name: processName,
        tasks: [
          {
            id: "charge",
            action: {
              type: "fetch" as const,
              url: `http://localhost:${charge.port}/action`,
            },
            only_once: true,
            timeout: 120_000,
            on_error: [{ code: ["only_once.interrupted"], goto: "$verify" }],
            switch: [{ goto: "end" }],
          },
          {
            id: "verify",
            action: {
              type: "fetch" as const,
              url: `http://localhost:${verify.port}/charges`,
            },
            switch: [{ goto: "end" }],
          },
        ],
      },
    });

    const { data: startData } = await genroc1.client.POST("/instances", {
      body: { process: processName },
    });
    const instanceId = startData!.id;

    await Promise.race([
      charge.firstRequestReceived,
      new Promise<never>((_, reject) =>
        setTimeout(
          () => reject(new Error("mock never received first request")),
          15_000,
        ),
      ),
    ]);

    genroc1.crash();

    const genroc2 = await startGenroc(genrocBin, RECOVER2_PORT, db, crashPgDSN, 0);
    try {
      // First tick expires the abandoned lease and routes; the handler runs on the
      // next one, since a routed goto persists and ends the advance.
      await genroc2.client.POST("/tick", { body: { advance_ms: 12_000 } });
      await genroc2.client.POST("/tick", {});

      const finalStatus = await waitForInstance(
        instanceId,
        15_000,
        genroc2.client,
      );
      expect(finalStatus).toBe("completed");

      // The engine never re-sent the charge; the recovery is the verify call.
      expect(charge.requestCount()).toBe(1);
      expect(verify.requestCount()).toBe(1);
    } finally {
      genroc2.stop();
    }
  } finally {
    genroc1.crash();
    await charge.stop();
    await verify.stop();
  }
}, 60_000);

// The other half of the contract: having checked and found that the call did NOT happen,
// a definition may route back into the interrupted task and re-run it. The engine refuses
// to repeat the call on its own; the definition may, once it has established it is safe.
test("crash recovery — a handler may deliberately re-run the interrupted task", async () => {
  const db = crashPgDSN ? "" : join(tmpdir(), `genroc_crash_rerun_${Date.now()}.db`);

  const charge = await startMockService(0, {
    response: { done: true },
    firstRequestDelayMs: Infinity,
  });
  // The system of record says the charge never landed, so the handler sends it again.
  const verify = await startMockService(0, { response: { exists: false } });

  const genroc1 = await startGenroc(genrocBin, RERUN1_PORT, db, crashPgDSN);
  try {
    const processName = `crash_rerun_${crypto.randomUUID()}`;
    await genroc1.client.PUT("/definitions", {
      body: {
        name: processName,
        tasks: [
          {
            id: "charge",
            action: {
              type: "fetch" as const,
              url: `http://localhost:${charge.port}/action`,
            },
            only_once: true,
            timeout: 120_000,
            on_error: [{ code: ["only_once.interrupted"], goto: "$verify" }],
            switch: [{ goto: "end" }],
          },
          {
            id: "verify",
            action: {
              type: "fetch" as const,
              url: `http://localhost:${verify.port}/charges`,
              // Declared so the switch below can read the answer: self.result is
              // the action's raw result, and it has to be typed to be navigated.
              responses: { 200: {
                type: "object",
                properties: { exists: { type: "boolean" } },
                required: ["exists"],
              } },
            },
            switch: [
              { case: "self.result.exists == true", goto: "end" },
              { goto: "$charge" },
            ],
          },
        ],
      },
    });

    const { data: startData } = await genroc1.client.POST("/instances", {
      body: { process: processName },
    });
    const instanceId = startData!.id;

    await Promise.race([
      charge.firstRequestReceived,
      new Promise<never>((_, reject) =>
        setTimeout(
          () => reject(new Error("mock never received first request")),
          15_000,
        ),
      ),
    ]);

    genroc1.crash();

    const genroc2 = await startGenroc(genrocBin, RERUN2_PORT, db, crashPgDSN, 0);
    try {
      await genroc2.client.POST("/tick", { body: { advance_ms: 12_000 } }); // route
      await genroc2.client.POST("/tick", {}); // verify → back to charge
      await genroc2.client.POST("/tick", {}); // the deliberate re-run

      const finalStatus = await waitForInstance(
        instanceId,
        15_000,
        genroc2.client,
      );
      expect(finalStatus).toBe("completed");

      // Twice: genroc1's abandoned attempt, and the one the definition asked for
      // after checking. The only_once guard does not re-fire on an authored re-entry.
      expect(charge.requestCount()).toBe(2);
      expect(verify.requestCount()).toBe(1);
    } finally {
      genroc2.stop();
    }
  } finally {
    genroc1.crash();
    await charge.stop();
    await verify.stop();
  }
}, 60_000);

// The remaining routing shapes, driven through the same real crash. A rule that matches
// only_once.interrupted is an ordinary on_error rule, so everything on_error offers has to
// work here — matching by wildcard, terminal clauses, and completion — and the only way to
// know is to run each one.
//
// They share a port pair, sequentially: the pause tests above already reuse theirs.
async function interruptedRecovery(
  db: string,
  tasks: unknown[],
  chargeMock: { port: number; firstRequestReceived: Promise<void> },
) {
  const processName = `interrupted_${crypto.randomUUID()}`;
  const genroc1 = await startGenroc(genrocBin, RECOVER1_PORT, db, crashPgDSN);
  const { error } = await genroc1.client.PUT("/definitions", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: { name: processName, tasks } as any,
  });
  expect(error).toBeUndefined();

  const { data } = await genroc1.client.POST("/instances", {
    body: { process: processName },
  });
  const instanceId = data!.id;

  await Promise.race([
    chargeMock.firstRequestReceived,
    new Promise<never>((_, reject) =>
      setTimeout(() => reject(new Error("mock never received first request")), 15_000),
    ),
  ]);
  genroc1.crash();

  const genroc2 = await startGenroc(genrocBin, RECOVER2_PORT, db, crashPgDSN, 0);
  await genroc2.client.POST("/tick", { body: { advance_ms: 12_000 } });
  return {
    instanceId,
    genroc2,
    stop: () => {
      genroc1.crash();
      genroc2.stop();
    },
  };
}

function chargeTask(port: number, onError: unknown[]) {
  return {
    id: "charge",
    action: { type: "fetch" as const, url: `http://localhost:${port}/action` },
    only_once: true,
    timeout: 120_000,
    on_error: onError,
    switch: [{ goto: "end" }],
  };
}

test("crash recovery — a wildcard rule catches only_once.interrupted, and earlier rules still win by order", async () => {
  const db = crashPgDSN ? "" : join(tmpdir(), `genroc_crash_wild_${Date.now()}.db`);
  const charge = await startMockService(0, {
    response: { done: true },
    firstRequestDelayMs: Infinity,
  });
  const verify = await startMockService(0, { response: { exists: true } });

  const run = await interruptedRecovery(
    db,
    [
      // The first rule cannot match this code; the second one can, by wildcard.
      chargeTask(charge.port, [
        { code: ["http.%"], goto: "$wrong" },
        { code: ["only_once.%"], goto: "$verify" },
      ]),
      {
        id: "verify",
        action: { type: "fetch" as const, url: `http://localhost:${verify.port}/charges` },
        switch: [{ goto: "end" }],
      },
      { id: "wrong", switch: [{ goto: "end" }] },
    ],
    charge,
  );

  try {
    await run.genroc2.client.POST("/tick", {});
    expect(await waitForInstance(run.instanceId, 15_000, run.genroc2.client)).toBe(
      "completed",
    );
    // Reaching verify at all proves the second rule matched: the first would have
    // routed to a task that calls nothing.
    expect(verify.requestCount()).toBe(1);
    expect(charge.requestCount()).toBe(1);
  } finally {
    run.stop();
    await charge.stop();
    await verify.stop();
  }
}, 60_000);

test("crash recovery — a bare catch-all rule catches only_once.interrupted", async () => {
  const db = crashPgDSN ? "" : join(tmpdir(), `genroc_crash_catchall_${Date.now()}.db`);
  const charge = await startMockService(0, {
    response: { done: true },
    firstRequestDelayMs: Infinity,
  });
  const verify = await startMockService(0, { response: { exists: true } });

  const run = await interruptedRecovery(
    db,
    [
      chargeTask(charge.port, [{ goto: "$verify" }]),
      {
        id: "verify",
        action: { type: "fetch" as const, url: `http://localhost:${verify.port}/charges` },
        switch: [{ goto: "end" }],
      },
    ],
    charge,
  );

  try {
    await run.genroc2.client.POST("/tick", {});
    expect(await waitForInstance(run.instanceId, 15_000, run.genroc2.client)).toBe(
      "completed",
    );
    expect(verify.requestCount()).toBe(1);
    expect(charge.requestCount()).toBe(1);
  } finally {
    run.stop();
    await charge.stop();
    await verify.stop();
  }
}, 60_000);

test("crash recovery — a handler may end the process, and its output is still computed", async () => {
  const db = crashPgDSN ? "" : join(tmpdir(), `genroc_crash_end_${Date.now()}.db`);
  const charge = await startMockService(0, {
    response: { done: true },
    firstRequestDelayMs: Infinity,
  });

  const processName = `interrupted_end_${crypto.randomUUID()}`;
  const genroc1 = await startGenroc(genrocBin, RECOVER1_PORT, db, crashPgDSN);
  try {
    const { error } = await genroc1.client.PUT("/definitions", {
      body: {
        name: processName,
        output: { recovered: true },
        tasks: [chargeTask(charge.port, [{ code: ["only_once.interrupted"], goto: "end" }])],
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
      } as any,
    });
    expect(error).toBeUndefined();

    const { data } = await genroc1.client.POST("/instances", {
      body: { process: processName },
    });
    const instanceId = data!.id;
    await charge.firstRequestReceived;
    genroc1.crash();

    const genroc2 = await startGenroc(genrocBin, RECOVER2_PORT, db, crashPgDSN, 0);
    try {
      await genroc2.client.POST("/tick", { body: { advance_ms: 12_000 } });
      expect(await waitForInstance(instanceId, 15_000, genroc2.client)).toBe("completed");

      // goto: end goes through completeViaErrorHandler, which computes the process
      // output like any other end — an anticipated interruption is a normal finish.
      const { data: after } = await genroc2.client.GET("/instances/{id}/detail", {
        params: { path: { id: instanceId } },
      });
      expect((after!.state as Record<string, unknown>).output).toEqual({
        recovered: true,
      });
      expect(charge.requestCount()).toBe(1);
    } finally {
      genroc2.stop();
    }
  } finally {
    genroc1.crash();
    await charge.stop();
  }
}, 60_000);

test("crash recovery — a handler may raise an authored code instead of routing", async () => {
  const db = crashPgDSN ? "" : join(tmpdir(), `genroc_crash_raise_${Date.now()}.db`);
  const charge = await startMockService(0, {
    response: { done: true },
    firstRequestDelayMs: Infinity,
  });

  const run = await interruptedRecovery(
    db,
    [
      chargeTask(charge.port, [
        {
          code: ["only_once.interrupted"],
          raise: {
            code: "charge_unconfirmed",
            message: "the charge may or may not have gone through",
          },
        },
      ]),
    ],
    charge,
  );

  try {
    expect(await waitForInstance(run.instanceId, 15_000, run.genroc2.client)).toBe(
      "raised",
    );
    const { data: after } = await run.genroc2.client.GET("/instances/{id}/detail", {
      params: { path: { id: run.instanceId } },
    });
    // The authored code is what an operator filters on; the engine's own code stays
    // visible in `error`, which is the point of raising rather than failing.
    expect(after!.error_code).toBe("charge_unconfirmed");
    expect(
      (after!.state as Record<string, Record<string, unknown>>).error?.code,
    ).toBe("only_once.interrupted");
    expect(charge.requestCount()).toBe(1);
  } finally {
    run.stop();
    await charge.stop();
  }
}, 60_000);
