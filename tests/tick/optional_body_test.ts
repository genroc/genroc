/**
 * POST /tick is the one HTTP endpoint whose request body reaches decodeOptionalBody
 * directly — every other optional-body action has a fromHTTP that rebuilds the payload
 * from query parameters, discarding whatever the client sent. So it is the only place
 * the "optional means absent, not unparseable" rule is observable over HTTP.
 *
 * The failure this pins down is specific: advance_ms sent as a string used to decode to
 * 0, leaving the server clock unmoved while the response still said 200. A test written
 * that way silently never advanced time, then asserted against timers that never fired.
 *
 * Needs manual-tick mode (--poll 0); on a polling server /tick answers 501 before it
 * ever looks at the body.
 */
import { expect, test } from "vitest";
import { useTickEnv } from "./helpers.ts";

const PORT = 20080;
useTickEnv(PORT);

async function post(body?: unknown) {
  const res = await fetch(`http://localhost:${PORT}/api/tick`, {
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
