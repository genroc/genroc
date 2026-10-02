import { expect, test } from "vitest";
import { client } from "../helpers/client.ts";

// Through /definitions/batch, the dedup path `genctl apply` uses.
test("run follows the latest channel after a content-dedup to an older version", async () => {
  const name = `dedup_latest_${crypto.randomUUID()}`;
  const contentA = { name, output: "A", tasks: [{ id: "t", switch: "end" }] };
  const contentB = { name, output: "B", tasks: [{ id: "t", switch: "end" }] };
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const apply = (def: any) =>
    client.PUT("/definitions/batch", { body: { definitions: [def], channel: "latest" } as any });

  await apply(contentA); // v1
  await apply(contentB); // v2 (highest version number)
  await apply(contentA); // dedups to v1; "latest" channel → v1

  const { data, error } = await client.POST("/instances", { body: { process: name } });
  expect(error).toBeUndefined();
  // Must be v1 (the channel target), not v2 (the highest version number).
  expect(data!.version).toBe(1);
});

test("'latest' channel is created on first apply even to a custom channel", async () => {
  const name = `ensure_latest_${crypto.randomUUID()}`;
  const a = { name, output: "A", tasks: [{ id: "t", switch: "end" }] };
  const b = { name, output: "B", tasks: [{ id: "t", switch: "end" }] };
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const applyTo = (def: any, channel: string) =>
    client.PUT("/definitions/batch", { body: { definitions: [def], channel } as any });

  await applyTo(a, "staging"); // v1; "latest" created → v1
  await applyTo(b, "staging"); // v2; "latest" already exists (v1), not updated

  const { data, error } = await client.POST("/instances", { body: { process: name } });
  expect(error).toBeUndefined();
  expect(data!.version).toBe(1);
});
