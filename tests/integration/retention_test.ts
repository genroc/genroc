import { expect, test } from "vitest";
import { startGenroc } from "../helpers/server.ts";

// --instance-retention, over HTTP: what an API caller sees once a concluded process is pruned.

test("retention — a concluded process answers 404 once past --instance-retention, a running one stays", async () => {
  const g = await startGenroc({ poll: 0, args: ["--instance-retention", "1h"] });
  try {
    const name = `retention_${crypto.randomUUID()}`;
    await g.client.PUT("/definitions", {
      body: { definitions: [{
        name,
        input_schema: { type: "object", properties: { wait: { type: "boolean" } } },
        tasks: [
          {
            id: "branch",
            switch: [{ case: "input.wait ?? false", goto: "$wait" }, { goto: "end" }],
          },
          { id: "wait", action: { type: "delay" as const, for: "1d" }, switch: [{ goto: "end" }] },
        ],
      }] },
    });
    const start = async (wait: boolean) =>
      (await g.client.POST("/instances", { body: { process: name, input: { wait } } })).data!.id;
    const done = await start(false);
    const waiting = await start(true);
    await g.client.POST("/tick", {});
    const status = async (id: string) => {
      const { data, response } = await g.client.GET("/instances/{id}", { params: { path: { id } } });
      return response.status === 404 ? 404 : data!.status;
    };
    expect(await status(done)).toBe("completed");
    expect(await status(waiting)).toBe("running");

    await g.client.POST("/tick", { body: { advance_ms: 30 * 60_000 } });
    expect(await status(done), "pruned before its retention ran out").toBe("completed");

    await g.client.POST("/tick", { body: { advance_ms: 60 * 60_000 } });
    expect(await status(done), "a concluded process outlived --instance-retention").toBe(404);
    expect(await status(waiting), "a running process must never be pruned").toBe("running");
  } finally {
    await g.stop();
  }
}, 60_000);

test("retention — off by default: nothing concluded is ever deleted without the flag", async () => {
  const g = await startGenroc({ poll: 0 });
  try {
    const name = `retention_default_${crypto.randomUUID()}`;
    await g.client.PUT("/definitions", { body: { definitions: [{ name, tasks: [{ id: "only", switch: [{ goto: "end" }] }] }] } });
    const id = (await g.client.POST("/instances", { body: { process: name } })).data!.id;
    await g.client.POST("/tick", {});
    await g.client.POST("/tick", { body: { advance_ms: 10 * 365 * 24 * 3_600_000 } }); // ten years
    const { data, response } = await g.client.GET("/instances/{id}", { params: { path: { id } } });
    expect(response.status, "deleting is opt-in (--instance-retention); a default must never delete").toBe(200);
    expect(data!.status).toBe("completed");
  } finally {
    await g.stop();
  }
}, 60_000);
