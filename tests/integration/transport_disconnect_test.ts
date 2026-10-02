import { expect, test } from "vitest";
import { createServer, type AddressInfo, type Socket } from "node:net";
import { client, waitForInstance } from "../helpers/client.ts";

// Takes delivery, then resets without answering — indistinguishable from "acted, then died
// answering", so never pre.*. On only_once, `delivered` IS the contract: nothing may increase it.
async function startVanishingRemote() {
  let delivered = 0;
  const open = new Set<Socket>();
  const server = createServer((socket) => {
    open.add(socket);
    socket.on("error", () => {}); // the reset we cause is delivered back to us
    socket.once("data", () => {
      delivered++;
      socket.resetAndDestroy(); // RST, the same answer a dead keep-alive peer gives
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  return {
    port: (server.address() as AddressInfo).port,
    get delivered() {
      return delivered;
    },
    async close() {
      for (const s of open) s.destroy();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    },
  };
}

async function runToCompletion(name: string, task: Record<string, unknown>) {
  const { error } = await client.PUT("/definitions", { body: { name, tasks: [task] } as never });
  if (error) throw new Error(`register failed: ${JSON.stringify(error)}`);
  const { data: started } = await client.POST("/instances", { body: { process: name } });
  const id = started!.id;
  const status = await waitForInstance(id);
  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  return { status, error: (data?.state?.last_error ?? {}) as Record<string, unknown> };
}

test("transport — a remote that vanishes mid-call reports http.disconnected, not pre.error", async () => {
  const remote = await startVanishingRemote();
  try {
    const { status, error } = await runToCompletion(`disconnect_${crypto.randomUUID()}`, {
      id: "call",
      action: { type: "fetch" as const, method: "post", url: `http://localhost:${remote.port}/eval`, timeout: 2000 },
      switch: [{ goto: "end" }],
    });

    expect(status).toBe("failed");
    expect(
      error.code,
      "the remote took delivery of the request, so pre.* would assert something the client " +
        "cannot know — a reset while awaiting the response is indistinguishable from a remote " +
        "that acted and died answering",
    ).toBe("http.disconnected");
    expect(remote.delivered).toBe(1);
  } finally {
    await remote.close();
  }
});

// A pre.%-only retry is the one an only_once task may declare, so it must not fire for a call the
// remote received.
test("transport — only_once does not re-send a call the remote received", async () => {
  const remote = await startVanishingRemote();
  try {
    const { status } = await runToCompletion(`disconnect_once_${crypto.randomUUID()}`, {
      id: "charge",
      only_once: true,
      action: { type: "fetch" as const, method: "post", url: `http://localhost:${remote.port}/charge`, timeout: 2000 },
      on_error: [{ code: ["pre.%"], retry: { retries: 2, delay: 50 } }],
      switch: [{ goto: "end" }],
    });

    expect(status).toBe("failed");
    expect(
      remote.delivered,
      "the charge was delivered more than once: an only_once task retried a call whose " +
        "outcome it cannot know, which is the exact guarantee only_once exists to make",
    ).toBe(1);
  } finally {
    await remote.close();
  }
});
