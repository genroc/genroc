import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// A fetch answers with more than a body. self.status and self.headers are siblings of
// self.result — the body keeps its meaning, nothing is re-wrapped — and they are what let a
// definition branch on which status arrived instead of routing a healthy 202 through
// on_error as a failure.
test("self.status / self.headers — readable beside the body", async () => {
  const svc = await startMockService(0, { statusCode: 202, response: { job: "j-1" } });

  const name = `selfmeta_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "kick",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${svc.port}/jobs`,
            accepted_status: ["200", "202"],
            responses: { "2xx": { type: "object", properties: { job: { type: "string" } } } },
          },
          output: {
            status: "$: self.status",
            // Lowercased by the transport: Go canonicalises to Content-Type, and a
            // canonicalised map would make this read null.
            ctype: "$: self.headers['content-type']",
            // Any key may be absent, so a header read is `string | null`.
            missing: "$: self.headers['x-nope'] == null",
            job: "$: self.result.job",
          },
          timeout: 2000,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  expect(putErr).toBeUndefined();

  const { data: started } = await client.POST("/instances", { body: { process: name } });
  const id = started!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  expect((data?.state?.outputs as any)?.kick).toEqual({
    status: 202,
    ctype: "application/json",
    missing: true,
    job: "j-1",
  });

  svc.stop();
});

// The gate: only a fetch answers with a status, so no other action type grows the slot.
// An always-null self.status on a delay would be a slot every context has to carry.
test("self.status — does not exist on a non-fetch task", async () => {
  const name = `selfmeta_gate_${crypto.randomUUID()}`;
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "wait",
          action: { type: "delay" as const, for: "1ms" },
          output: { s: "$: self.status" },
          switch: [{ goto: "end" }],
        },
      ],
    } as any,
  });
  expect(error, "self.status must not resolve on a delay task").toBeDefined();
});
