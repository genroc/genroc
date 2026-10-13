import { afterAll, beforeAll, expect, test } from "vitest";
import { startGenroc, tmpPath, type GenrocProcess } from "../helpers/server.ts";

const MAX = 4096;

let server: GenrocProcess;

beforeAll(async () => {
  server = await startGenroc({ db: tmpPath("logpayloadmax", ".db"), args: ["--log-payload-max", String(MAX)] });
}, 90_000);

afterAll(async () => {
  await server?.stop();
});

async function completedEntry(blob: string, name: string) {
  const { data: started } = await server.client.POST("/instances", { body: { process: name, input: { blob } } });
  const id = started!.id;
  for (let i = 0; i < 200; i++) {
    const { data } = await server.client.GET("/instances/{id}", { params: { path: { id } } });
    if (data?.status !== "running") break;
    await new Promise((r) => setTimeout(r, 50));
  }
  const { data: logs } = await server.client.GET("/instances/{id}/logs", {
    params: { path: { id }, query: { limit: 100 } },
  });
  const entry = (logs!.items ?? []).find((l) => l.event === "inst_completed");
  expect(entry, "the completion is logged").toBeDefined();
  return entry!;
}

test("a payload over --log-payload-max is left out with its size; one under it is kept whole", async () => {
  const name = `logmax_${crypto.randomUUID()}`;
  const { error } = await server.client.PUT("/definitions", {
    body: {
      name,
      input_schema: { type: "object", properties: { blob: { type: "string" } }, required: ["blob"] },
      tasks: [{ id: "t", switch: [{ goto: "end" }] }],
      output: { echo: "$: input.blob" },
    } as never,
  });
  expect(error, `put failed: ${JSON.stringify(error)}`).toBeUndefined();

  const big = await completedEntry("B".repeat(10_000), name);
  expect(big.data, "a payload over the maximum is left out, never stored cut short").toBeUndefined();
  expect(big.objects, "nothing of it reaches the object store").toBeUndefined();
  expect((big.meta as any)?.payload_omitted_bytes, "the entry says how big the left-out payload was")
    .toBeGreaterThan(MAX);

  // Over the 2 KiB limit but under the maximum: cut into the object store like any value.
  const mid = "M".repeat(3_000);
  const kept = await completedEntry(mid, name);
  expect((kept.meta as any)?.payload_omitted_bytes, "a payload under the maximum is not left out").toBeUndefined();
  expect(kept.data, "its large part is null in place").toEqual({ echo: null });
  const ref = kept.objects?.find((o) => o.path?.join(".") === "data.echo")?.ref;
  expect(ref, "and listed in objects").toBeDefined();
  const { data: obj } = await server.client.GET("/objects/{ref}", { params: { path: { ref: ref! } } });
  expect(JSON.parse((obj as any).data), "the stored payload is complete").toBe(mid);
});
