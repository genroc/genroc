import { createServer, type ServerResponse } from "http";
import type { AddressInfo } from "net";
import { afterAll, beforeAll, expect, test } from "vitest";
import {
  buildGenrocBinary,
  startGenroc,
  tmpPath,
  type GenrocProcess,
} from "../helpers/server.ts";

// The sleeping-laptop case over HTTP: tick #1 parks mid-fetch on a blocking mock, /tick
// advance_ms sleeps past the lease, a concurrent tick #2 wakes and reclaims. Fence half
// only — Tick has no gate. specs/lease-fencing.md, "The e2e layer".

const PORT = 20051;

// Parks the FIRST request until release() and answers all later ones instantly, so a
// tick can be held in flight while the test moves the clock under it.
async function blockingMock() {
  let hits = 0;
  let parked: ServerResponse | undefined;
  let arrived!: () => void;
  const firstArrived = new Promise<void>((r) => (arrived = r));
  const respond = (res: ServerResponse) => {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ ok: true }));
  };
  const server = createServer((_req, res) => {
    hits++;
    if (hits === 1) {
      parked = res;
      arrived();
      return;
    }
    respond(res);
  });
  await new Promise<void>((r) => server.listen(0, r));
  return {
    port: (server.address() as AddressInfo).port,
    hits: () => hits,
    firstArrived,
    release: () => {
      if (parked) {
        respond(parked);
        parked = undefined;
      }
    },
    stop: () => new Promise<void>((r) => server.close(() => r())),
  };
}

let genroc: GenrocProcess;

beforeAll(async () => {
  const bin = await buildGenrocBinary();
  const db = tmpPath("genroc_fence", ".db");
  // poll=0 → manual ticks; max-concurrent 4 so the reclaiming tick is not starved of a
  // slot by the one the parked advance holds.
  genroc = await startGenroc(bin, PORT, db, undefined, 0, 4, true);
}, 60_000);

afterAll(() => genroc?.stop());

async function leaseLostLogged(id: string): Promise<boolean> {
  const { data } = await genroc.client.GET("/instances/{id}/logs", {
    params: { path: { id } },
  });
  return (data?.items ?? []).some((l) => l.event === "lease_lost");
}

async function instanceState(id: string) {
  const { data, error } = await genroc.client.GET("/instances/{id}", {
    params: { path: { id } },
  });
  expect(error).toBeUndefined();
  return data!;
}

function defineAndStart(mockPort: number, onlyOnce: boolean) {
  const name = `fence_${onlyOnce ? "oo" : "plain"}_${crypto.randomUUID()}`;
  return (async () => {
    const { error } = await genroc.client.PUT("/definitions", {
      body: {
        name,
        tasks: [
          {
            id: "charge",
            ...(onlyOnce ? { only_once: true } : {}),
            action: { type: "fetch" as const, url: `http://localhost:${mockPort}/x` },
            switch: [{ goto: "end" }],
          },
        ],
      } as never,
    });
    expect(error).toBeUndefined();
    const { data, error: startErr } = await genroc.client.POST("/instances", {
      body: { process: name },
    });
    expect(startErr).toBeUndefined();
    return data!.id;
  })();
}

test("only_once: the takeover verdict survives the frozen tick's write, endpoint hit exactly once", async () => {
  const mock = await blockingMock();
  try {
    const id = await defineAndStart(mock.port, true);

    // Tick #1 claims and blocks in the fetch — the worker frozen mid-charge.
    const tick1 = genroc.client.POST("/tick", {});
    await mock.firstArrived;

    // The laptop sleeps an hour; on wake, tick #2 reclaims the expired lease. It must
    // adjudicate (only_once.interrupted), never re-issue the charge.
    const { data: t2 } = await genroc.client.POST("/tick", {
      body: { advance_ms: 3_600_000 },
    });
    expect((t2 as { count: number }).count).toBe(1);
    let inst = await instanceState(id);
    expect(inst.status).toBe("failed");
    expect(inst.error_code).toBe("only_once.interrupted");
    expect(mock.hits()).toBe(1);

    // The frozen worker wakes and finishes the call; its "completed" must be refused.
    mock.release();
    await tick1;
    expect(await leaseLostLogged(id)).toBe(true);

    inst = await instanceState(id);
    expect(inst.status, "the stale write clobbered the takeover verdict").toBe("failed");
    expect(inst.error_code).toBe("only_once.interrupted");
    expect(mock.hits(), "the charge went out more than once").toBe(1);
  } finally {
    mock.release();
    await mock.stop();
  }
}, 30_000);

test("plain task: the reclaim re-runs it and the stale write cannot clobber the fresh result", async () => {
  const mock = await blockingMock();
  try {
    const id = await defineAndStart(mock.port, false);

    const tick1 = genroc.client.POST("/tick", {});
    await mock.firstArrived;

    // At-least-once for a plain task: the reclaiming tick re-runs the fetch (hit #2,
    // answered instantly) and completes the instance.
    const { data: t2 } = await genroc.client.POST("/tick", {
      body: { advance_ms: 3_600_000 },
    });
    expect((t2 as { count: number }).count).toBe(1);
    let inst = await instanceState(id);
    expect(inst.status).toBe("completed");
    expect(mock.hits()).toBe(2);

    mock.release();
    await tick1;
    expect(await leaseLostLogged(id)).toBe(true);

    inst = await instanceState(id);
    expect(inst.status, "the stale write reopened a completed instance").toBe("completed");
    expect(inst.error_code ?? "").toBe("");
  } finally {
    mock.release();
    await mock.stop();
  }
}, 30_000);
