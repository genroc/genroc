import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

// `end` and `next` are not reserved task ids, and never needed to be: a routing slot tells a
// task from a keyword by the `$` sigil, which is the whole reason the sigil is there. So
// `goto: $end` reaches a task named `end` and `goto: end` terminates — in the same definition.
//
// Validation used to refuse the id outright. This runs the process, because a rule can be
// dropped from the validator and still be baked into how the engine resolves a target.

test("a task may be named `end`, and `$end` routes to it while `end` terminates", async () => {
  const name = `reserved_end_${crypto.randomUUID().replace(/-/g, "")}`;
  await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        // Two ways out of one task: the keyword and the task that shares its spelling.
        { id: "first", output: { step: "first" }, switch: [{ goto: "$end" }] },
        { id: "next", output: { step: "next" }, switch: [{ goto: "end" }] },
        { id: "end", output: { step: "end" }, switch: [{ goto: "$next" }] },
      ],
      output: {
        reached_end: "$: outputs.end.step",
        reached_next: "$: outputs.next.step",
      },
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });

  const { data: startData } = await client.POST("/instances", { body: { process: name } });
  const id = startData!.id;
  expect(await waitForInstance(id)).toBe("completed");

  const { data } = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  // first → $end → the task named `end` → $next → the task named `next` → end (the keyword).
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  expect((data?.state as any)?.output).toEqual({ reached_end: "end", reached_next: "next" });
});

// The sigil disambiguates a keyword from a task; it does not conjure a task that is not there.
test("`$end` with no task named end is still rejected at registration", async () => {
  const name = `reserved_missing_${crypto.randomUUID().replace(/-/g, "")}`;
  const { response } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [{ id: "first", switch: [{ goto: "$end" }] }],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(response.status).toBe(400);
});

// Uniqueness is a separate question the sigil does NOT answer: with two tasks spelled the same,
// `$tick` reaches one of them and `outputs.tick` names one of them, and nothing says which.
test("two tasks sharing an id are rejected", async () => {
  const name = `duplicate_id_${crypto.randomUUID().replace(/-/g, "")}`;
  const { response } = await client.PUT("/definitions", {
    body: {
      name,
      tasks: [
        { id: "tick", switch: [{ goto: "next" }] },
        { id: "tick", switch: [{ goto: "end" }] },
      ],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });
  expect(response.status).toBe(400);
});
