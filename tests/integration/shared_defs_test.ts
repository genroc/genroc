import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// Process-level $defs: a definition declared once is referenced from both the
// input_schema and a result_schema. Input validation resolves it (default fill +
// pruning), and the output map reads a typed field through the shared $ref.
test("process-level $defs are shared by input_schema and result_schemas", async () => {
  const mock = await startMockService(0, {
    response: { buyer: { name: "al", vip: true, junk: "dropped" } },
  });

  const name = `shared_defs_${crypto.randomUUID()}`;
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      $defs: {
        User: {
          type: "object",
          properties: {
            name: { type: "string" },
            vip: { type: "boolean", default: false },
          },
          required: ["name"],
        },
      },
      input_schema: {
        type: "object",
        properties: { requester: { $ref: "#/$defs/User" } },
        required: ["requester"],
      },
      tasks: [
        {
          id: "fetch",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${mock.port}/action`,
            responses: { 200: {
              type: "object",
              properties: { buyer: { $ref: "#/$defs/User" } },
              required: ["buyer"],
            } },
          },
          output: { who: "$: self.result.buyer.name" },
          switch: [{ goto: "end" }],
        },
      ],
      output: { who: "$: outputs.fetch.who", requester: "$: input.requester.name" },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(error).toBeUndefined();

  // Input goes through the shared def: vip default fills, undeclared keys prune.
  const { data: startData } = await client.POST("/instances", {
    body: { process: name, input: { requester: { name: "bo", extra: 1 } } },
  });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const ctx = data?.state as any;
  expect(ctx?.input?.requester).toEqual({ name: "bo", vip: false });
  expect(ctx?.output).toEqual({ who: "al", requester: "bo" });

  mock.stop();
});

// A schema may BE a bare $ref to a process-level def — including one named
// "input", colliding with the generated schema name. The colliding def is
// renamed, the resulting alias chain resolves for inference and validation,
// and defaults fill through it.
test("input_schema as a bare $ref to a def named 'input' works", async () => {
  const name = `shared_defs_bare_ref_${crypto.randomUUID()}`;
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      $defs: {
        input: {
          type: "object",
          properties: {
            blob: { type: "string", default: "12" },
            sleep: { type: "integer" },
          },
          required: ["sleep"],
        },
      },
      input_schema: { $ref: "#/$defs/input" },
      tasks: [{ id: "route", output: "$: input.blob", switch: "end" }],
      output: "$: outputs.route",
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(error).toBeUndefined();

  // blob is omitted: its default must fill through the bare root $ref.
  const { data: startData } = await client.POST("/instances", {
    body: { process: name, input: { sleep: 1 } },
  });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((data?.state as any)?.output).toBe("12");
});

// Generated schema names take precedence by renaming: a user definition named
// like a generated one (here fetch_output) is accepted — generation renames it
// with a unique suffix and rewrites the $refs pointing at it, so the process
// registers and runs with correct typing through the renamed definition.
test("$defs colliding with generated schema names are safely renamed", async () => {
  const mock = await startMockService(0, {
    response: { d: { n: 7 } },
  });

  const name = `shared_defs_renamed_${crypto.randomUUID()}`;
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      $defs: {
        fetch_output: { type: "object", properties: { n: { type: "integer" } }, required: ["n"] },
      },
      tasks: [
        {
          id: "fetch",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${mock.port}/action`,
            responses: { 200: {
              type: "object",
              properties: { d: { $ref: "#/$defs/fetch_output" } },
              required: ["d"],
            } },
          },
          output: { num: "$: self.result.d.n" },
          switch: [{ goto: "end" }],
        },
      ],
      output: { num: "$: outputs.fetch.num" },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(error).toBeUndefined();

  const { data: startData } = await client.POST("/instances", { body: { process: name } });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((data?.state as any)?.output).toEqual({ num: 7 });

  mock.stop();
});
