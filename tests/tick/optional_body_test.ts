/** Optional means absent, not unparseable: advance_ms sent as a string once decoded to 0, so the
 *  clock never moved while /tick answered 200. Needs --poll 0. */
import { expect, test } from "vitest";
import { useTickEnv } from "./helpers.ts";

const ctx = useTickEnv();

async function post(body?: unknown) {
  const res = await fetch(`${ctx.env.baseUrl}/api/tick`, {
    method: "POST",
    ...(body === undefined
      ? {}
      : { headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) }),
  });
  return { status: res.status, body: (await res.json()) as Record<string, unknown> };
}

test("optional body — an absent body is still the zero value", async () => {
  const { status } = await post();
  expect(status).toBe(200);
});

test("optional body — an empty object is accepted", async () => {
  const { status } = await post({});
  expect(status).toBe(200);
});

test("optional body — a valid advance_ms is accepted", async () => {
  const { status } = await post({ advance_ms: 1000 });
  expect(status).toBe(200);
});

test("optional body — a wrong-typed advance_ms is rejected, not silently zeroed", async () => {
  const { status, body } = await post({ advance_ms: "1000" });
  expect(status).toBe(400);
  expect(body.code).toBe("invalid");
});

test("optional body — a misspelled field is rejected, not dropped", async () => {
  const { status, body } = await post({ advnace_ms: 1000 });
  expect(status).toBe(400);
  expect(body.code).toBe("invalid");
  expect(body.error).toContain("advnace_ms");
});

test("optional body — a negative advance_ms is still rejected as invalid", async () => {
  const { status, body } = await post({ advance_ms: -1 });
  expect(status).toBe(400);
  expect(body.code).toBe("invalid");
});
