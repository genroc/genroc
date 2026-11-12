import { expect, test } from "vitest";
import { client, runToEnd } from "../helpers/client.ts";

// `x-` keys belong to whoever wrote them: accepted wherever genroc's own structure is, stored as
// written, returned with the definition, and never read by the engine. specs/extension-keys.md.

function extended(name: string, zoom: number) {
  return {
    name,
    "x-builder": { zoom, nodes: [{ id: "a", at: [10, 20] }] },
    input_schema: {
      type: "object",
      "x-s": "form",
      properties: { a: { type: "string", "x-order": 3 } },
    },
    tasks: [{ id: "a", "x-pos": { x: 10, y: 20 }, switch: [{ goto: "end", "x-case": "happy path" }] }],
    // A key of the VALUE: data the process outputs, not an extension.
    output: { "x-id": "$: 'computed'" },
  };
}

async function readBack(name: string, version?: number) {
  const { data, error } = await client.GET("/definitions/{name}", {
    params: { path: { name }, query: version === undefined ? {} : { version } },
  });
  expect(error, `GET /definitions/${name}`).toBeUndefined();
  // `x-` keys are not part of the model's type: they are the author's, read here as written.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  return data as unknown as { name: string; version: number; definition: any };
}

test("x- keys are stored as written and returned with the definition", async () => {
  const name = `xkeys_${crypto.randomUUID().slice(0, 8)}`;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const { error } = await client.PUT("/definitions", { body: { definitions: [extended(name, 1) as any] } });
  expect(error).toBeUndefined();

  const got = await readBack(name);
  expect(got.name).toBe(name);
  expect(got.version).toBe(1);
  const d = got.definition;
  expect(d["x-builder"], "a builder's layout comes back as it was written").toEqual({ zoom: 1, nodes: [{ id: "a", at: [10, 20] }] });
  expect(d.tasks[0]["x-pos"]).toEqual({ x: 10, y: 20 });
  expect(d.tasks[0].switch[0]["x-case"]).toBe("happy path");
  expect(d.input_schema["x-s"]).toBe("form");
  expect(d.input_schema.properties.a["x-order"]).toBe(3);
});

test("a change only to x- keys is a new version with the same runtime, each keeping its document", async () => {
  const name = `xkeys_v_${crypto.randomUUID().slice(0, 8)}`;
  const apply = async (zoom: number) => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const { data, error } = await client.PUT("/definitions", { body: { definitions: [extended(name, zoom) as any] } });
    expect(error).toBeUndefined();
    return data?.[0];
  };
  expect(await apply(1)).toMatchObject({ version: 1, saved: true, same_runtime: false });
  expect(await apply(2), "a version is a document, and nothing that runs differs from v1")
    .toMatchObject({ version: 2, saved: true, previous: 1, same_runtime: true });
  expect((await readBack(name)).definition["x-builder"].zoom).toBe(2);
  expect((await readBack(name, 1)).definition["x-builder"].zoom, "v1 never changes once written").toBe(1);
  expect(await apply(1), "an earlier document walks the pointer back to its version")
    .toMatchObject({ version: 1, saved: false });
});

test("an x- key inside a value is data the process outputs", async () => {
  const name = `xkeys_run_${crypto.randomUUID().slice(0, 8)}`;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  await client.PUT("/definitions", { body: { definitions: [extended(name, 1) as any] } });
  const { status, data } = await runToEnd(name, { a: "x" });
  expect(status).toBe("completed");
  expect(data?.output).toEqual({ "x-id": "computed" });
});

test("a key that only looks like an x- key is still refused", async () => {
  const name = `xkeys_bad_${crypto.randomUUID().slice(0, 8)}`;
  const { error } = await client.PUT("/definitions", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: { definitions: [{ name, tasks: [{ id: "a", switch: "end", x_pos: 1 }] } as any] },
  });
  expect(JSON.stringify(error ?? {})).toContain('unknown field \\"x_pos\\"');
});

test("reading a name nothing is registered under is not_found", async () => {
  const { error, response } = await client.GET("/definitions/{name}", {
    params: { path: { name: `xkeys_none_${crypto.randomUUID().slice(0, 8)}` } },
  });
  expect(response.status).toBe(404);
  expect(error?.code).toBe("not_found");
});
