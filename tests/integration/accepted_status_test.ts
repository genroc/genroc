import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

// accepted_status is a shape evaluating to an array of HTTP status patterns. A status it
// covers is treated as success; anything else stays an http.NNN error. It can be authored
// as a literal array or as a $: expression resolved per request (the "ease the validation"
// change: it is no longer a statically pattern-checked []string).

function fetchDef(name: string, url: string, acceptedStatus?: unknown) {
  const action: Record<string, unknown> = { type: "fetch", method: "post", url };
  if (acceptedStatus !== undefined) action.accepted_status = acceptedStatus;
  return { name, tasks: [{ id: "call", action, switch: "end" }] };
}

test("accepted_status literal array — a covered non-2xx counts as success", async () => {
  const mock = await startMockService(0, { statusCode: 404, response: { note: "gone" } });
  const name = `acc_lit_${crypto.randomUUID()}`;

  const { error: putErr } = await client.PUT("/definitions", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: fetchDef(name, `http://localhost:${mock.port}/action`, ["404"]) as any,
  });
  expect(putErr).toBeUndefined();

  const { data, error } = await client.POST("/instances", { body: { process: name } });
  expect(error).toBeUndefined();
  expect(await waitForInstance(data!.id)).toBe("completed");

  mock.stop();
});

test("without accepted_status — a 404 fails (control)", async () => {
  const mock = await startMockService(0, { statusCode: 404, response: { note: "gone" } });
  const name = `acc_ctl_${crypto.randomUUID()}`;

  await client.PUT("/definitions", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: fetchDef(name, `http://localhost:${mock.port}/action`) as any,
  });
  const { data } = await client.POST("/instances", { body: { process: name } });
  expect(await waitForInstance(data!.id)).toBe("failed");

  mock.stop();
});

test("accepted_status as a $: expression — resolved from input per request", async () => {
  // A 418 is covered by the "4xx" range pattern the expression yields.
  const mock = await startMockService(0, { statusCode: 418, response: { teapot: true } });
  const name = `acc_expr_${crypto.randomUUID()}`;

  await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: { codes: { type: "array", items: { type: "string" } } },
        required: ["codes"],
      },
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch",
            method: "post",
            url: `http://localhost:${mock.port}/action`,
            accepted_status: "$: input.codes",
          },
          switch: "end",
        },
      ],
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any,
  });

  const { data } = await client.POST("/instances", {
    body: { process: name, input: { codes: ["4xx"] } },
  });
  expect(await waitForInstance(data!.id)).toBe("completed");

  mock.stop();
});

test("accepted_status must be an array of strings — a non-string array is rejected at registration", async () => {
  const name = `acc_bad_${crypto.randomUUID()}`;
  const { error } = await client.PUT("/definitions", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: fetchDef(name, "http://localhost/x", [404, 500]) as any,
  });
  // The structural array<string> check still holds; only the per-pattern format was eased.
  expect(error).toBeDefined();
  expect(JSON.stringify(error)).toContain("accepted_status");
});

test("a static (literal) status pattern is still format-checked at registration", async () => {
  const name = `acc_fmt_${crypto.randomUUID()}`;
  const { error } = await client.PUT("/definitions", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: fetchDef(name, "http://localhost/x", ["2xx", "banana"]) as any,
  });
  // "banana" is a static literal, so its format is checked even though accepted_status is
  // now a shape — only dynamic (expression) elements skip the format check.
  expect(error).toBeDefined();
  expect(JSON.stringify(error)).toContain("banana");
});
