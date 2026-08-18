import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// The output map remaps an action's result: result_schema validates the full
// response, but only the projection is exported to outputs.<task> (coupling
// reduction), and the switch routes on the remapped self.output.
test("output map remaps an action result — only the projection is exported", async () => {
  const mock = await startMockService(0, {
    response: { job_id: "j-42", queue: "q1", secret: "shh" },
  });

  const name = `output_remap_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "create",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${mock.port}/action`,
            responses: { 200: {
              type: "object",
              properties: {
                job_id: { type: "string" },
                queue: { type: "string" },
                secret: { type: "string" },
              },
              required: ["job_id"],
            } },
          },
          output: { id: "$: self.result.job_id" },
          switch: [
            { case: `self.output.id == "j-42"`, goto: "end" },
            { goto: "end" },
          ],
        },
      ],
      output: { id: "$: outputs.create.id" },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });

  const { data: startData } = await client.POST("/instances", { body: { process: name } });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  // Only the projected {id} is exported — not the full {job_id, queue, secret} body.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((data?.context?.outputs as any)?.create).toEqual({ id: "j-42" });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((data?.context as any)?.output?.id).toBe("j-42");

  mock.stop();
});

// A single-expression output ("$: self.result") passes the action result
// through unchanged, with no object wrapper. The process output is also a single
// expression that forwards the task output.
test("single-expression output passes the action result through", async () => {
  const mock = await startMockService(0, {
    response: { job_id: "j-7", queue: "q1" },
  });

  const name = `output_passthrough_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "create",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${mock.port}/action`,
            responses: { 200: {
              type: "object",
              properties: { job_id: { type: "string" }, queue: { type: "string" } },
              required: ["job_id", "queue"],
            } },
          },
          output: "$: self.result",
          switch: [{ case: `self.output.job_id == "j-7"`, goto: "end" }, { goto: "end" }],
        },
      ],
      output: "$: outputs.create",
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });

  const { data: startData } = await client.POST("/instances", { body: { process: name } });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  // The whole result is exported (passthrough), and the process output forwards it.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((data?.context?.outputs as any)?.create).toEqual({ job_id: "j-7", queue: "q1" });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((data?.context as any)?.output).toEqual({ job_id: "j-7", queue: "q1" });

  mock.stop();
});

// A nested-object output shapes the data freely: nested objects with expression
// leaves are evaluated recursively.
test("nested output shapes data with nested objects", async () => {
  const mock = await startMockService(0, {
    response: { job_id: "j-9", queue: "q2" },
  });

  const name = `output_nested_${crypto.randomUUID()}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        {
          id: "create",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${mock.port}/action`,
            responses: { 200: {
              type: "object",
              properties: { job_id: { type: "string" }, queue: { type: "string" } },
              required: ["job_id", "queue"],
            } },
          },
          output: { meta: { id: "$: self.result.job_id", where: "$: self.result.queue" } },
          switch: "end",
        },
      ],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });

  const { data: startData } = await client.POST("/instances", { body: { process: name } });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}", { params: { path: { id } } });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((data?.context?.outputs as any)?.create).toEqual({
    meta: { id: "j-9", where: "q2" },
  });

  mock.stop();
});
