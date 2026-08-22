/**
 * Registration-time rules for an on_error rule's `retry` policy.
 *
 * Every rejection here exists because the alternative is silent: a policy that decodes to
 * something the author did not write still matches errors and still routes, so the only
 * symptom is a retry that never happens or a wait that is nothing like the one requested.
 */
import { expect, test } from "vitest";
import { client, waitForInstance } from "../helpers/client.ts";

async function define(
  retry: unknown,
  task: Record<string, unknown> = {},
  def: Record<string, unknown> = {},
) {
  const name = `retry_policy_${crypto.randomUUID()}`;
  const { error } = await client.PUT("/definitions", {
    body: {
      name,
      ...def,
      tasks: [
        {
          id: "call",
          action: { type: "fetch" as const, url: "http://localhost:19990/x" },
          on_error: [{ code: ["http.5%"], retry }],
          switch: "end",
          ...task,
        },
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
      ] as any,
    },
  });
  return { name, error: error?.error as string | undefined };
}

test("retry — the pre-policy \"retries\" key is rejected by name", async () => {
  const { error } = await client.PUT("/definitions", {
    body: {
      name: `retry_old_key_${crypto.randomUUID()}`,
      tasks: [
        {
          id: "call",
          action: { type: "fetch" as const, url: "http://localhost:19990/x" },
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
          on_error: [{ code: ["http.5%"], retries: 3 } as any],
          switch: "end",
        },
      ],
    },
  });
  // Accepting it silently would leave a rule that still matches and still routes, and
  // never retries — so the message has to name the replacement, not just the bad key.
  expect(error?.error).toContain('unknown field "retries"');
  expect(error?.error).toContain('renamed to "retry"');
});

test("retry — scalar and object forms are both accepted", async () => {
  expect((await define(3)).error).toBeUndefined();
  expect(
    (await define({ attempts: 4, delay: "30s", factor: 2, max_delay: "10m" }))
      .error,
  ).toBeUndefined();
});

test("retry — max_delay below delay is rejected", async () => {
  const { error } = await define({
    attempts: 3,
    delay: "10m",
    max_delay: "30s",
  });
  expect(error).toContain("shorter than retry.delay");
});

test("retry — a backoff with no attempts is rejected", async () => {
  const { error } = await define({ delay: "30s" });
  expect(error).toContain("never retry");
});

test("retry — a factor below 1 is rejected", async () => {
  const { error } = await define({ attempts: 3, factor: 0.5 });
  expect(error).toContain("shrink the wait");
});

test("retry — calendar units are rejected in a duration", async () => {
  // The curve scales the value and compares it to a ceiling; "1mo" has no length until a
  // timezone and a start instant fix it.
  const { error } = await define({ attempts: 3, delay: "1d" });
  expect(error).toContain("calendar units");
});

test("retry — a zero delay is rejected", async () => {
  const { error } = await define({ attempts: 3, delay: 0 });
  expect(error).toContain("positive");
});

test("retry — an unknown key inside the policy is rejected", async () => {
  const { error } = await define({ attempts: 3, backoff: "30s" });
  expect(error).toContain('unknown field "backoff"');
});

test("retry — a quoted attempt count is rejected", async () => {
  // The shorthand is typed `integer` in the published schema, so the server must not
  // accept what an editor validating against that schema would flag.
  const { error } = await define("3");
  expect(error).toContain("quoted");
});

test("retry — a duration that overflows the nanosecond counter is rejected", async () => {
  // Not merely large: "5124096h" used to wrap to a positive 25 minutes, which every
  // downstream check accepts. A retry would have fired 292 years early, silently.
  const { error } = await define({ attempts: 3, delay: "5124096h" });
  expect(error).toContain("out of range");
});

// ── Expression-valued slots ───────────────────────────────────────────────────
//
// Every slot of a policy also accepts a $: expression, so the curve can come from config
// and be tuned per environment (a k8s cold start is minutes; a laptop is seconds). The
// value does not exist at registration, which is what the checks below are about.

const WHO = {
  input_schema: {
    type: "object",
    properties: { who: { type: "string" } },
  },
};

test("retry — a $: slot must evaluate to a number", async () => {
  const { error } = await define({ attempts: "$: input.who" }, {}, WHO);
  expect(error).toContain("must evaluate to a number");
  expect(error).toContain("retry.attempts");
});

test("retry — a ${ } interpolation is refused, since it produces a string", async () => {
  // eslint-disable-next-line no-template-curly-in-string
  const { error } = await define({ attempts: "${ input.who }" }, {}, WHO);
  expect(error).toContain("not a number");
});

test("retry — a $: policy is accepted", async () => {
  const { error } = await define(
    {
      attempts: "$: config.e2e_retry_attempts",
      delay: "$: config.e2e_retry_delay_ms",
    },
    {},
    {
      config_schema: {
        type: "object",
        properties: {
          e2e_retry_attempts: { type: "integer", default: 1 },
          e2e_retry_delay_ms: { type: "integer", default: 100 },
        },
      },
    },
  );
  expect(error).toBeUndefined();
});

// The one that matters: the attempt count actually comes from the environment. The schema
// default is 0, so any retry at all proves the env var reached the curve rather than the
// slot decoding to a number nobody wrote.
test("retry — the environment drives how many attempts are made", async () => {
  const name = `retry_from_env_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      config_schema: {
        type: "object",
        properties: {
          // GENROC_GLOBAL_E2E_RETRY_ATTEMPTS is set to 2 on the test server; 0 here means
          // a policy that never retries, which is what a lost expression would produce.
          e2e_retry_attempts: { type: "integer", default: 0 },
        },
      },
      tasks: [
        {
          id: "call",
          // Nothing listens here, so every attempt fails pre.error — the code a cold
          // start produces, and the one that is safe to retry.
          action: { type: "fetch" as const, url: "http://localhost:19991/gone" },
          on_error: [
            {
              code: ["pre.%"],
              retry: { attempts: "$: config.e2e_retry_attempts", delay: 50 },
            },
          ],
          switch: "end",
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
        } as any,
      ],
    },
  });
  expect(putErr).toBeUndefined();

  const { data: started } = await client.POST("/instances", {
    body: { process: name, input: {} },
  });
  const status = await waitForInstance(started!.id, 20_000);
  expect(status).toBe("failed");

  const { data: logs } = await client.GET("/instances/{id}/logs", {
    params: { path: { id: started!.id } },
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const items = ((logs as any)?.items ?? []) as Array<{ event: string; message?: string }>;
  const scheduled = items.filter((e) => e.event === "retry_scheduled");
  expect(scheduled).toHaveLength(2);
  // The counter in the message is the resolved budget, not the authored source.
  expect(scheduled.some((e) => e.message?.includes("(attempt 2/2)"))).toBe(true);
});
