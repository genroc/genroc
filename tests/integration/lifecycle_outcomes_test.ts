import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";
import { BASE_URL } from "../helpers/constants.ts";

// pause and resume are ASSERTIONS — "make this tree paused", "make this tree advance" —
// so an assertion that already holds is a success that changed nothing, not a 409. The
// status line carries which of the three it was, because two of the three transports
// (TCP, UDS) have no status line and read the same fact off Reply.outcome.
// specs/id-list-commands.md.

const MISSING_ID = "00000000-0000-0000-0000-000000000000";

async function post(path: string) {
  const res = await fetch(`${BASE_URL}/api${path}`, { method: "POST" });
  const text = await res.text();
  return { status: res.status, body: text ? (JSON.parse(text) as Record<string, unknown>) : null };
}

async function apply(def: object & { name: string }) {
  const { error } = await client.PUT("/definitions", { body: def as never });
  expect(error, `apply ${def.name}`).toBeUndefined();
  return def.name;
}

/** A pause on a leased row lands only when that worker's write releases the lease. */
async function waitForStatus(id: string, want: string) {
  for (let i = 0; i < 200; i++) {
    const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
    if (data?.status === want) return;
    await new Promise((r) => setTimeout(r, 25));
  }
  throw new Error(`${id} never reached ${want}`);
}

async function start(name: string) {
  const { data, error } = await client.POST("/instances", { body: { process: name, input: {} } });
  expect(error).toBeUndefined();
  return data!.id;
}

/** Parks on an external task, so the tree sits live and non-terminal indefinitely. */
function parkedDef(name: string) {
  return {
    name,
    tasks: [
      {
        id: "hold",
        action: {
          type: "external" as const,
          input: {},
          result_schema: { type: "object", properties: { ok: { type: "boolean" } } },
        },
        switch: [{ goto: "end" }],
      },
    ],
  };
}

function instantDef(name: string) {
  return { name, tasks: [{ id: "t", switch: [{ goto: "end" }] }] };
}

test("pause — 200/202 when it acts, 204 unchanged once the tree has come to rest", async () => {
  const id = await start(await apply(parkedDef(`out_pause_${crypto.randomUUID().slice(0, 8)}`)));

  // 202 rather than 200 when a worker holds the task: whether it does is a race against
  // the poll loop, so both are accepted here and the distinction is pinned deterministically
  // in TestPauseProcess_OutcomeAcceptedWhileDraining.
  const first = await post(`/instances/${id}/pause`);
  expect([200, 202]).toContain(first.status);
  expect(first.body!.outcome === "applied" || first.body!.outcome === "accepted").toBe(true);
  expect(first.body!.instances, "an acting outcome must say how much it wrote").toBeGreaterThan(0);

  await waitForStatus(id, "paused");

  // The assertion now holds. Not an error: this is what lets a half-applied group of ids
  // be re-run as-is, which is the whole reason the outcome exists.
  const again = await post(`/instances/${id}/pause`);
  expect(again.status).toBe(204);
  expect(again.body, "204 must not carry a body").toBeNull();
});

test("pause — a settled tree is 204, never 409: it is not advancing, which is the assertion", async () => {
  const id = await start(await apply(instantDef(`out_settled_${crypto.randomUUID().slice(0, 8)}`)));
  expect(await waitForInstance(id)).toBe("completed");

  expect((await post(`/instances/${id}/pause`)).status).toBe(204);
});

test("resume — 204 when the tree is already advancing, 409 only when it has settled", async () => {
  const live = await start(await apply(parkedDef(`out_live_${crypto.randomUUID().slice(0, 8)}`)));

  // Live and unpaused: "make this tree advance" already holds.
  const already = await post(`/instances/${live}/resume`);
  expect(already.status).toBe(204);

  await post(`/instances/${live}/pause`);
  await waitForStatus(live, "paused");
  const resumed = await post(`/instances/${live}/resume`);
  expect(resumed.status).toBe(200);
  expect(resumed.body).toMatchObject({ outcome: "applied", status: "running" });

  // Settled is the other side of the split, and the one the CLI could not make for
  // itself: the assertion can never hold, so it stays a refusal the operator must answer.
  const done = await start(await apply(instantDef(`out_done_${crypto.randomUUID().slice(0, 8)}`)));
  expect(await waitForInstance(done)).toBe("completed");
  const settled = await post(`/instances/${done}/resume`);
  expect(settled.status).toBe(409);
  expect(settled.body!.code).toBe("conflict");
  expect(settled.body!.error).toContain("settled");
});

test("retry — not an assertion, so every refusal stays a 409 and it never reports unchanged", async () => {
  const id = await start(await apply(instantDef(`out_retry_${crypto.randomUUID().slice(0, 8)}`)));
  expect(await waitForInstance(id)).toBe("completed");

  const res = await post(`/instances/${id}/retry`);
  expect(res.status, "no prior state satisfies 'was given a fresh attempt'").toBe(409);
  expect(res.body!.code).toBe("conflict");
});

test("an unchanged assertion writes nothing — no updated_at bump, or the outcome is decorative", async () => {
  const id = await start(await apply(parkedDef(`out_noop_${crypto.randomUUID().slice(0, 8)}`)));
  await post(`/instances/${id}/pause`);
  await waitForStatus(id, "paused");

  const before = (await client.GET("/instances/{id}", { params: { path: { id } } })).data!;
  await new Promise((r) => setTimeout(r, 25));
  expect((await post(`/instances/${id}/pause`)).status).toBe(204);
  const after = (await client.GET("/instances/{id}", { params: { path: { id } } })).data!;

  expect(after.updated_at, "a no-op must not touch the row").toBe(before.updated_at);
});

test("a missing instance is still 404 — an unknown id is a mistake, not a satisfied assertion", async () => {
  for (const verb of ["pause", "resume", "retry"]) {
    const res = await post(`/instances/${MISSING_ID}/${verb}`);
    expect(res.status, verb).toBe(404);
    expect(res.body!.code).toBe("not_found");
  }
});
