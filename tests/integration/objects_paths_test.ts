import { expect, test } from "vitest";
import { client, fetchObject, waitForInstance } from "../helpers/client.ts";
import { claimInProcess, waitForParked } from "../helpers/external.ts";

// Every response that lists objects promises the same thing: each path names a location in THAT
// response. The per-endpoint tests check one root apiece with a literal; this checks the promise
// itself, on every root at once, so a root added later is covered without anyone remembering to.
//
// It exists because the ordinary splice cannot fail: spliceObjects walks to the parent and writes
// only `if (cur)`, so a path naming a slot the response does not have is a silent no-op. A
// listing can therefore be wrong in a way that every splicing test still passes.

const BLOB = "P".repeat(8 * 1024);

type Entry = { path: (string | number)[]; ref: string; size: number };

/** Walks to the parent of `path`, or explains where it ran out. */
function parentAt(body: unknown, path: (string | number)[]): unknown {
  let cur: any = body;
  for (let i = 0; i < path.length - 1; i++) {
    if (cur === null || cur === undefined) return undefined;
    cur = cur[path[i]];
  }
  return cur;
}

/**
 * The three things a listing owes its reader, asserted together: every path lands somewhere,
 * no path is listed twice, and splicing each one puts the value back where the response holds it.
 */
async function assertListingIsPlaceable(where: string, body: unknown) {
  const objects = ((body as { objects?: Entry[] }).objects ?? []) as Entry[];
  expect(objects.length, `${where}: the fixture must externalize something, or this proves nothing`)
    .toBeGreaterThan(0);

  const seen = new Set<string>();
  for (const e of objects) {
    const printed = e.path.join(".");
    expect(seen.has(printed), `${where}: ${printed} is listed twice — a splice would write it into two places`)
      .toBe(false);
    seen.add(printed);

    const parent = parentAt(body, e.path);
    expect(parent, `${where}: ${printed} names a location this response does not have`).toBeDefined();
    expect(typeof parent, `${where}: ${printed} — the parent is not a container`).toBe("object");

    // The leaf must be ABSENT rather than a marker: a reference sitting where a value goes is
    // what the listing exists to avoid.
    const leaf = (parent as Record<string, unknown>)[e.path[e.path.length - 1]];
    expect(leaf, `${where}: ${printed} still holds something inline`).toBeUndefined();

    const value = JSON.parse(await fetchObject(e.ref));
    (parent as Record<string, unknown>)[e.path[e.path.length - 1]] = value;
  }
}

async function define(name: string, body: Record<string, unknown>) {
  const { error } = await client.PUT("/definitions", { body: { name, ...body } as never });
  expect(error, `define ${name}`).toBeUndefined();
}

test("every objects path on the status and detail views names a place in that response", async () => {
  const name = `objpath_done_${crypto.randomUUID().slice(0, 8)}`;
  await define(name, {
    input_schema: { type: "object", properties: { blob: { type: "string" } }, required: ["blob"] },
    tasks: [{ id: "only", output: { echo: "$: input.blob" }, switch: "end" }],
    output: { echo: "$: outputs.only.echo" },
  });
  const { data: started } = await client.POST("/instances", { body: { process: name, input: { blob: BLOB } } });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data: status } = await client.GET("/instances/{id}", { params: { path: { id } } });
  await assertListingIsPlaceable("GET /instances/{id}", status);
  expect((status!.output as Record<string, unknown>).echo, "spliced from the listing alone").toBe(BLOB);

  const { data: detail } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  await assertListingIsPlaceable("GET /instances/{id}/detail", detail);
  expect((detail!.output as Record<string, unknown>).echo).toBe(BLOB);
  expect(((detail!.state as any).input as Record<string, unknown>).blob).toBe(BLOB);
});

test("every objects path on a raised instance's error payload names a place in that response", async () => {
  const name = `objpath_raise_${crypto.randomUUID().slice(0, 8)}`;
  await define(name, {
    input_schema: { type: "object", properties: { blob: { type: "string" } }, required: ["blob"] },
    tasks: [{ id: "boom", switch: [{ panic: { code: "kaput", message: "bang", data: { trace: "$: input.blob" } } }] }],
  });
  const { data: started } = await client.POST("/instances", { body: { process: name, input: { blob: BLOB } } });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("failed");

  const { data: status } = await client.GET("/instances/{id}", { params: { path: { id } } });
  await assertListingIsPlaceable("GET /instances/{id} (raised)", status);
  expect((status!.error_data as Record<string, unknown>).trace).toBe(BLOB);

  const { data: detail } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  await assertListingIsPlaceable("GET /instances/{id}/detail (raised)", detail);
  expect((detail!.error_data as Record<string, unknown>).trace).toBe(BLOB);
});

test("every objects path on a parked task names a place — on both views and on the claim", async () => {
  const name = `objpath_ext_${crypto.randomUUID().slice(0, 8)}`;
  await define(name, {
    input_schema: { type: "object", properties: { blob: { type: "string" } }, required: ["blob"] },
    tasks: [
      {
        id: "hold",
        action: { type: "external", input: { payload: "$: input.blob" }, result_schema: {} },
        output: { r: "$: self.result" },
        switch: "end",
      },
    ],
  });
  const { data: started } = await client.POST("/instances", { body: { process: name, input: { blob: BLOB } } });
  const id = started!.id;
  await waitForParked(id);

  const { data: status } = await client.GET("/instances/{id}", { params: { path: { id } } });
  await assertListingIsPlaceable("GET /instances/{id} (parked)", status);
  expect((status!.external_input as Record<string, unknown>).payload).toBe(BLOB);

  const { data: detail } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  await assertListingIsPlaceable("GET /instances/{id}/detail (parked)", detail);
  expect((detail!.external_input as Record<string, unknown>).payload).toBe(BLOB);

  // The claim is the fourth listing, and the only one whose paths address an ENTRY rather than
  // the instance -- so it is the one a change to the instance views can silently skew.
  const [entry] = await claimInProcess(name);
  await assertListingIsPlaceable("POST /external-tasks/claim", entry);
  expect((entry.external_input as Record<string, unknown>).payload).toBe(BLOB);
});

test("every objects path on a log entry names a place in that entry", async () => {
  const name = `objpath_log_${crypto.randomUUID().slice(0, 8)}`;
  await define(name, {
    input_schema: { type: "object", properties: { blob: { type: "string" } }, required: ["blob"] },
    tasks: [{ id: "only", output: { echo: "$: input.blob" }, switch: "end" }],
    output: { echo: "$: outputs.only.echo" },
  });
  const { data: started } = await client.POST("/instances", { body: { process: name, input: { blob: BLOB } } });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data: logs } = await client.GET("/instances/{id}/logs", { params: { path: { id } } });
  const carrying = ((logs!.items ?? []) as Record<string, unknown>[]).filter(
    (l) => ((l.objects ?? []) as Entry[]).length > 0,
  );
  expect(carrying.length, "a log row must carry an object, or this proves nothing").toBeGreaterThan(0);
  // Each ROW owns its section, which is what keeps a path stable while pages accumulate.
  for (const row of carrying) await assertListingIsPlaceable(`log row ${String(row.event)}`, row);
});
