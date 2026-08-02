import { expect, test } from "vitest";
import { client } from "../helpers/client.ts";
import { BASE_URL } from "../helpers/constants.ts";

// /healthz is what a container supervisor probes. Its contract is narrow on purpose: 200
// means this worker reached its database, and the rest of the body is operator context
// that must never turn a working worker into a failing probe.

test("health — a worker that can reach its database answers 200 ok", async () => {
  const { data, error } = await client.GET("/healthz");
  expect(error).toBeUndefined();
  expect(data?.status).toBe("ok");
  expect(data?.worker).toBeTruthy();
  expect(["sqlite", "postgres"]).toContain(data?.database);
});

test("health — lease_age_ms reports a live renewal, not a zero-value epoch", async () => {
  // Seeded in engine.New as well as engine.Run: a probe landing in the gap between them
  // used to read the zero value and report ~56 years of staleness.
  const { data } = await client.GET("/healthz");
  expect(data?.lease_age_ms).toBeGreaterThanOrEqual(0);
  expect(data?.lease_age_ms).toBeLessThan(60_000);
});

test("health — the probe needs no body and no content type", async () => {
  const res = await fetch(`${BASE_URL}/healthz`);
  expect(res.status).toBe(200);
  expect(res.headers.get("content-type")).toContain("application/json");
});
