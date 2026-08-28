import { client as defaultClient } from "./client.ts";

// The external-task LISTING was removed: it was the polling shape that claim/renew/release
// replaced, and everything a caller needs is derivable from the instance it is parked on.
// A resolve token is `<instance-id>.<task_epoch>` (model.ExternalToken), and the rest of what
// the listing carried — the task id, the evaluated input snapshot, who holds a claim — is on
// GET /instances/{id}/detail. What is NOT recoverable is `result_schema`; only the listing
// published that, and nothing asserts on it any more.

export type ParkedTask = {
  token: string;
  task_id?: string;
  input?: unknown;
  claimed_by?: string;
  claim_expires?: string;
};

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type Client = { GET: any };

/** The parked external task on `id`, or undefined when it is not parked on one. */
export async function parkedTask(id: string, c: Client = defaultClient): Promise<ParkedTask | undefined> {
  const { data } = await c.GET("/instances/{id}/detail", { params: { path: { id } } });
  if (!data || data.wait_state !== "external") return undefined;
  return {
    token: `${id}.${data.task_epoch}`,
    task_id: data.task,
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    input: (data.state as any)?._external?.input,
    claimed_by: data.external_worker_id,
    claim_expires: data.external_lease_expires_at,
  };
}

/** Polls until `id` is parked on an external task. Throws rather than returning undefined, so
 *  a test that never parks fails where it waited instead of on a later null dereference. */
export async function waitForParked(id: string, c: Client = defaultClient, timeoutMs = 10_000): Promise<ParkedTask> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const t = await parkedTask(id, c);
    if (t) return t;
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error(`instance ${id} never parked on an external task within ${timeoutMs}ms`);
}

/** The token for a parked instance, as `genctl resolve` takes it. */
export async function tokenFor(id: string, c: Client = defaultClient): Promise<string> {
  return (await waitForParked(id, c)).token;
}

/**
 * Every task parked on an external wait within one process, discovered through the INSTANCES
 * listing — which is where fleet-wide discovery lives now that the external-task listing is
 * gone. `wait_state` comes back on each row, so the filter is client-side.
 */
export async function parkedInProcess(
  process: string,
  c: Client = defaultClient,
): Promise<ParkedTask[]> {
  const { data } = await c.GET("/instances", { params: { query: { process, status: "running" } } });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const rows = ((data as any)?.items ?? []) as any[];
  const out: ParkedTask[] = [];
  for (const r of rows.filter((r) => r.wait_state === "external")) {
    const t = await parkedTask(r.id, c);
    if (t) out.push(t);
  }
  return out;
}

/** Polls until `count` tasks are parked in `process`. `beforeEach` drives a tick-only server. */
export async function waitForParkedInProcess(
  process: string,
  opts: { client?: Client; count?: number; timeoutMs?: number; beforeEach?: () => Promise<unknown> } = {},
): Promise<ParkedTask[]> {
  const { client: c = defaultClient, count = 1, timeoutMs = 20_000, beforeEach } = opts;
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (beforeEach) await beforeEach();
    const found = await parkedInProcess(process, c);
    if (found.length >= count) return found;
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error(`${process}: ${count} parked external task(s) not seen within ${timeoutMs}ms`);
}

/**
 * Claim the parked tasks of one process, which is how a worker learns what it must answer
 * with: the claim response — not discovery — publishes `result_schema`, `raises` and the
 * `objects` list. Returns the claimed entries verbatim.
 */
export async function claimInProcess(
  process: string,
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  opts: { client?: any; worker?: string; limit?: number } = {},
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
): Promise<any[]> {
  const { client: c = defaultClient, worker = `probe-${Math.random().toString(36).slice(2, 8)}`, limit = 10 } = opts;
  const { data } = await c.POST("/external-tasks/claim", { body: { worker_id: worker, limit, process } });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const items = ((data as any)?.items ?? []) as any[];
  // Hand them straight back: a claim taken only to inspect must not hold work away from a
  // real worker for its full lease.
  for (const t of items) await c.POST("/external-tasks/release", { body: { token: t.token } });
  return items;
}
