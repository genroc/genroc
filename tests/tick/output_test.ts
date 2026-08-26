import { expect, test } from "vitest";
import { useTickEnv } from "./helpers.ts";

// Exercises the per-task `output` map end to end: a no-action task computes its
// output from self.previous (recursive, inferred — no schema declared), the
// switch routes on self.output, and the remapped value is what later tasks and
// the process output see.
const ctx = useTickEnv(20021);

test("no-action output map drives a counter via self.previous; switch reads self.output", async () => {
  await ctx.env.client.PUT("/definitions", {
    body: {
      name: "out_counter",
      tasks: [
        {
          id: "count",
          output: { n: "$: (self.previous.n ?? 0) + 1" },
          switch: [
            { case: "self.output.n >= 3", goto: "end" },
            { goto: "$count" }, // loop until the bound
          ],
        },
      ],
      output: { n: "$: outputs.count.n" },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });

  const id = await ctx.env.start("out_counter");
  await ctx.env.tickUntilIdle();

  expect(await ctx.env.status(id)).toBe("completed");
  const { data } = await ctx.env.client.GET("/instances/{id}/detail", {
    params: { path: { id } },
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((data!.state as any)?.output?.n).toBe(3);
});

test("cross-task mutual recursion (start <-> loop) type-checks and runs", async () => {
  // start reads loop's output and loop reads start's, closed by a goto loop —
  // a mutual cycle resolved by the joint SCC fixpoint at validation time.
  await ctx.env.client.PUT("/definitions", {
    body: {
      name: "cross_loop",
      input_schema: { type: "object", properties: { ttl: { type: "integer" } }, required: ["ttl"] },
      tasks: [
        { id: "start", output: { num: "$: outputs.loop.num" }, switch: "next" },
        {
          id: "loop",
          output: { num: "$: (outputs.start.num ?? 0) + 1" },
          switch: [
            { case: "self.output.num < input.ttl", goto: "$start" },
            { goto: "end" },
          ],
        },
      ],
      output: { num: "$: outputs.start.num" },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });

  const { data: startData } = await ctx.env.client.POST("/instances", {
    body: { process: "cross_loop", input: { ttl: 3 } },
  });
  const id = startData!.id;
  await ctx.env.tickUntilIdle();

  expect(await ctx.env.status(id)).toBe("completed");
  const { data } = await ctx.env.client.GET("/instances/{id}/detail", {
    params: { path: { id } },
  });
  // loop counts 1,2,3 (stops at >= ttl); start mirrors the prior loop value → 2.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((data!.state as any)?.output?.num).toBe(2);
});
