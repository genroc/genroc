import { expect, test } from "vitest";
import { startGenroc, tmpPath, type GenrocProcess } from "../helpers/server.ts";
import { startMockService, waitForInstance } from "../helpers/client.ts";

// SIGTERM stops claiming at once, lets running work finish for --shutdown-grace, and abandons
// the rest unwritten: a deploy must never record itself as the task's failure.

type Mock = Awaited<ReturnType<typeof startMockService>>;

async function startCall(g: GenrocProcess, mock: Mock, onlyOnce = false): Promise<string> {
  const name = `shutdown_${crypto.randomUUID()}`;
  await g.client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "call",
          ...(onlyOnce ? { only_once: true } : {}),
          action: {
            type: "fetch" as const,
            method: "post",
            url: `http://localhost:${mock.port}/action`,
            timeout: 120_000,
          },
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  const { data } = await g.client.POST("/instances", { body: { process: name } });
  await Promise.race([
    mock.firstRequestReceived,
    new Promise<never>((_, reject) => setTimeout(() => reject(new Error("the call never started")), 15_000)),
  ]);
  return data!.id;
}

async function events(g: GenrocProcess, id: string): Promise<string[]> {
  const { data } = await g.client.GET("/instances/{id}/logs", {
    params: { path: { id }, query: { limit: 100 } },
  });
  return (data!.items ?? []).map((l) => l.event as string);
}

test("shutdown — a call in flight at SIGTERM finishes and is recorded", async () => {
  const db = tmpPath("genroc_shutdown", ".db");
  const mock = await startMockService(0, { response: { done: true }, firstRequestDelayMs: 1_500 });
  const g1 = await startGenroc({ db });
  let g2: GenrocProcess | undefined;
  try {
    const id = await startCall(g1, mock);
    await g1.stop();

    g2 = await startGenroc({ db, poll: 0 });
    const { data } = await g2.client.GET("/instances/{id}", { params: { path: { id } } });
    expect(data!.status, "a graceful stop cut the call off instead of letting it finish").toBe("completed");
    expect(mock.requestCount(), "the finished call must not be sent again").toBe(1);
  } finally {
    await g2?.stop();
    g1.crash();
    await mock.stop();
  }
}, 60_000);

test("shutdown — work outliving the grace is abandoned unwritten and re-run elsewhere", async () => {
  const db = tmpPath("genroc_shutdown_grace", ".db");
  const mock = await startMockService(0, { response: { done: true }, firstRequestDelayMs: Infinity });
  const g1 = await startGenroc({ db, args: ["--shutdown-grace", "300ms"] });
  let g2: GenrocProcess | undefined;
  try {
    const id = await startCall(g1, mock);
    await g1.stop();

    g2 = await startGenroc({ db, poll: 0 });
    await g2.client.POST("/tick", { body: { advance_ms: 12_000 } }); // past the 10s lease
    expect(await waitForInstance(id, 15_000, g2.client), "an abandoned task must re-run, not fail").toBe(
      "completed",
    );
    expect(mock.requestCount()).toBe(2);
    const trail = await events(g2, id);
    expect(trail, "the trail must say the work was abandoned").toContain("work_abandoned");
    expect(trail, "the cancel an abandoned call unwound through was logged as its failure").not.toContain(
      "action_failed",
    );
  } finally {
    mock.release();
    await g2?.stop();
    g1.crash();
    await mock.stop();
  }
}, 60_000);

test("shutdown — an abandoned only_once task raises only_once.interrupted, never re-runs", async () => {
  const db = tmpPath("genroc_shutdown_once", ".db");
  const mock = await startMockService(0, { response: { done: true }, firstRequestDelayMs: Infinity });
  const g1 = await startGenroc({ db, args: ["--shutdown-grace", "300ms"] });
  let g2: GenrocProcess | undefined;
  try {
    const id = await startCall(g1, mock, true);
    await g1.stop();

    g2 = await startGenroc({ db, poll: 0 });
    await g2.client.POST("/tick", { body: { advance_ms: 12_000 } });
    expect(await waitForInstance(id, 15_000, g2.client)).toBe("failed");
    const { data } = await g2.client.GET("/instances/{id}/detail", { params: { path: { id } } });
    expect(data!.error_code, "abandoning must leave the evidence a crash leaves").toBe("only_once.interrupted");
    expect(mock.requestCount(), "an only_once call was sent twice").toBe(1);
  } finally {
    mock.release();
    await g2?.stop();
    g1.crash();
    await mock.stop();
  }
}, 60_000);
