/**
 * Moving a process tree to another version of its definitions.
 *
 * The operation is tree-shaped on purpose: only the ROOT's target version is named, and
 * every live descendant's is derived from the definition its parent moves to. Naming a
 * child's version by hand is how a parent ends up running one its own definition never
 * mentions. specs/version-compatibility.md s3c.
 */
import { expect, test } from "vitest";
import { client } from "../helpers/client.ts";

/** Polls the instance until pred holds. The shared waitForInstance waits for terminal
 *  states; every state this file cares about is deliberately non-terminal. */
async function waitUntil(
  id: string,
  pred: (i: Record<string, unknown>) => boolean,
  timeoutMs = 5000,
) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
    if (data && pred(data as unknown as Record<string, unknown>)) return data;
    if (Date.now() > deadline) throw new Error(`instance ${id} never matched`);
    await new Promise((r) => setTimeout(r, 50));
  }
}

async function put(body: Record<string, unknown>) {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const { error } = await client.PUT("/definitions", { body: body as any });
  expect(error).toBeUndefined();
}

async function start(process: string, input: Record<string, unknown>) {
  const { data, error } = await client.POST("/instances", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: { process, input } as any,
  });
  expect(error).toBeUndefined();
  return data!.id;
}

async function upgrade(id: string, body: Record<string, unknown>) {
  return client.POST("/instances/{id}/upgrade", {
    params: { path: { id } },
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: body as any,
  });
}

test("a paused instance moves to a new version and its state is migrated", async () => {
  const name = `upg_${crypto.randomUUID().slice(0, 8)}`;
  // v1: `note` is optional. v2 requires it, and it admits null -- the gap a migration closes.
  await put({
    name,
    input_schema: { type: "object", properties: { note: { type: ["string", "null"] } } },
    tasks: [{ id: "hold", action: { type: "external" }, switch: "end" }],
  });
  const id = await start(name, {});
  await waitUntil(id, (i) => i.wait_state === "external");

  const paused = await client.POST("/instances/{id}/pause", { params: { path: { id } } });
  expect(paused.error).toBeUndefined();
  await waitUntil(id, (i) => i.status === "paused");

  await put({
    name,
    input_schema: {
      type: "object",
      properties: { note: { type: ["string", "null"] } },
      required: ["note"],
    },
    tasks: [{ id: "hold", action: { type: "external" }, switch: "end" }],
  });

  const done = await upgrade(id, { from_version: 1, to_version: 2 });
  expect(done.error).toBeUndefined();
  expect(done.data!.upgraded).toBe(true);

  const after = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  expect(after.data!.version).toBe(2);
  // The migration inserted the null v2 requires, so the row satisfies the version it now runs.
  expect(after.data!.state?.input).toHaveProperty("note", null);
});

test("a stale from_version is refused rather than migrated against a version it has left", async () => {
  const name = `upg_stale_${crypto.randomUUID().slice(0, 8)}`;
  await put({ name, tasks: [{ id: "hold", action: { type: "external" }, switch: "end" }] });
  const id = await start(name, {});
  await waitUntil(id, (i) => i.wait_state === "external");
  await client.POST("/instances/{id}/pause", { params: { path: { id } } });
  await waitUntil(id, (i) => i.status === "paused");

  await put({ name, tasks: [{ id: "hold", action: { type: "external" }, switch: "end" }] });

  const { error } = await upgrade(id, { from_version: 99, to_version: 2 });
  expect(error).toBeDefined();
});

test("a running instance is refused: it can be advanced between the plan and the write", async () => {
  const name = `upg_running_${crypto.randomUUID().slice(0, 8)}`;
  await put({ name, tasks: [{ id: "hold", action: { type: "external" }, switch: "end" }] });
  const id = await start(name, {});
  await waitUntil(id, (i) => i.wait_state === "external");
  await put({ name, tasks: [{ id: "hold", action: { type: "external" }, switch: "end" }] });

  const { data, error } = await upgrade(id, { to_version: 2 });
  expect(error).toBeUndefined();
  // Reported, not thrown: the caller needs to know WHICH member blocked the tree and why.
  expect(data!.upgraded).toBe(false);
  expect(data!.moves?.[0]?.reason ?? "").toMatch(/paused or failed/);
});
