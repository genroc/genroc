import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";
import { BASE_URL } from "../helpers/constants.ts";

// These assertions read the RAW response body rather than parsed JSON on purpose:
// JavaScript numbers are float64 too, so JSON.parse would corrupt the very values
// under test before the assertion ran. Comparing the bytes is the only honest
// check that the server preserved them.
async function rawOutput(id: string): Promise<string> {
  // The detail endpoint, because the values under test live in STATE: the status endpoint
  // reports where an instance ended, not what it holds.
  const res = await fetch(`${BASE_URL}/instances/${id}/detail`);
  return await res.text();
}

// A workflow that merely forwards a value must not change it. Under the old
// float64 pipeline this failed at json.Unmarshal, before any expression ran.
test("numbers — large integers survive a round trip untouched", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const name = `numpass_${uid}`;

  await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: { id: { type: "integer" }, amount: { type: "number" } },
        required: ["id", "amount"],
      },
      tasks: [{ id: "done", switch: [{ goto: "end" }] }],
      output: { id: "$: input.id", amount: "$: input.amount" },
    },
  });

  // 9007199254740993 is 2^53+1 — the smallest integer float64 cannot represent.
  const body = `{"process":"${name}","input":{"id":9007199254740993,"amount":123456789.123456789}}`;
  const started = await fetch(`${BASE_URL}/instances`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body,
  });
  const id = (await started.json()).id as string;

  expect(await waitForInstance(id, 10_000)).toBe("completed");

  const raw = await rawOutput(id);
  expect(raw).toContain("9007199254740993");
  expect(raw).not.toContain("9007199254740992");
  expect(raw).toContain("123456789.123456789");
});

// Arithmetic is exact base-10, so the classic binary-float artefacts do not
// appear: 0.1 + 0.2 is 0.3, not 0.30000000000000004.
test("numbers — decimal arithmetic is exact", async () => {
  const uid = crypto.randomUUID().slice(0, 8);
  const name = `nummath_${uid}`;

  await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: { a: { type: "number" }, b: { type: "number" }, big: { type: "integer" } },
        required: ["a", "b", "big"],
      },
      tasks: [{ id: "done", switch: [{ goto: "end" }] }],
      output: {
        sum: "$: input.a + input.b",
        exact: "$: input.a + input.b == 0.3",
        bigPlusOne: "$: input.big + 1",
        money: "$: input.a * 3",
      },
    },
  });

  const started = await fetch(`${BASE_URL}/instances`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: `{"process":"${name}","input":{"a":0.1,"b":0.2,"big":9007199254740993}}`,
  });
  const id = (await started.json()).id as string;

  expect(await waitForInstance(id, 10_000)).toBe("completed");

  const raw = await rawOutput(id);
  expect(raw).toContain(`"sum":0.3`);
  expect(raw).not.toContain("0.30000000000000004");
  expect(raw).toContain(`"exact":true`);
  expect(raw).toContain(`"bigPlusOne":9007199254740994`);
  // 0.1 * 3 is 0.30000000000000004 in float64.
  expect(raw).toContain(`"money":0.3`);
});
