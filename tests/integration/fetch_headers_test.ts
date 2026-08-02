import { createServer } from "http";
import type { AddressInfo } from "net";
import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// Captures the headers of the first request it receives.
async function captureHeaders() {
  let seen: Record<string, string | string[] | undefined> = {};
  const server = createServer((req, res) => {
    seen = req.headers;
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ ok: true }));
  });
  await new Promise<void>((r) => server.listen(0, r));
  return {
    port: (server.address() as AddressInfo).port,
    headers: () => seen,
    stop: () => new Promise<void>((r) => server.close(() => r())),
  };
}

// genroc stamps the caller's identity on every fetch request (the raw body carries no
// envelope), so the receiving service can correlate the call back to the instance/task.
test("fetch stamps X-Genroc-Instance-Id and X-Genroc-Task-Id, and caller headers coexist", async () => {
  const mock = await captureHeaders();
  try {
    const name = `fetch_headers_${crypto.randomUUID()}`;
    await client.PUT("/definitions", {
      body: {
        name,
        tasks: [
          {
            id: "call",
            action: {
              type: "fetch" as const,
              url: `http://localhost:${mock.port}/x`,
              headers: { "X-Trace": "abc" },
            },
            timeout: 2000,
            switch: [{ goto: "end" }],
          },
        ],
      } as never,
    });

    const { data: started } = await client.POST("/instances", { body: { process: name } });
    const id = started!.id;
    expect(await waitForInstance(id)).toBe("completed");

    const h = mock.headers(); // Node lower-cases header names
    expect(h["x-genroc-instance-id"]).toBe(id);
    expect(h["x-genroc-task-id"]).toBe("call");
    expect(h["x-trace"]).toBe("abc"); // caller header still present alongside the identity ones
  } finally {
    await mock.stop();
  }
});
