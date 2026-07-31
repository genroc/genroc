import { expect, test } from "vitest";
import { client } from "../helpers/client.ts";

// The `delay` action takes exactly one of `for` (a duration from arm time) or `until` (an
// instant). Each accepts three written forms, and which one it is decides how it is
// checked — a split made syntactically, before any type inference runs:
//
//   pure literal        for: "2h30m"                 parsed against the delayspec grammar
//   bare JSON number    for: 5000                    milliseconds (unix ms for until)
//   $: expression       until: "$: input.due_ms"     must infer to a number
//   ${ } interpolation  for: "${ input.h }h"         rejected — it yields a string at runtime
//
// The grammars themselves (DST, month-end clamping, calendar patterns) are table-tested in
// internal/delayspec; these cases cover the registration surface over HTTP.

const delayDef = (action: Record<string, unknown>) => ({
  name: `delay_syntax_${crypto.randomUUID().replace(/-/g, "")}`,
  input_schema: {
    type: "object",
    properties: { hours: { type: "integer" }, due_ms: { type: "integer" } },
    required: ["hours", "due_ms"],
  },
  tasks: [{ id: "wait", action: { type: "delay", ...action }, switch: [{ goto: "end" }] }],
});

const accepted: Array<[string, Record<string, unknown>]> = [
  ["for: duration literal", { for: "2h30m" }],
  ["for: multi-unit literal with space", { for: "1d 12h" }],
  ["for: calendar month literal", { for: "3mo" }],
  ["for: sub-second literal", { for: "500ms" }],
  ["for: bare number is milliseconds", { for: 5000 }],
  ["for: $: expression", { for: "$: input.hours * 3600000" }],
  ["for: literal with tz", { for: "1d", tz: "Europe/Prague" }],
  ["for: literal with fixed-offset tz", { for: "1d", tz: "+02:00" }],
  ["until: offset and wall clock", { until: "+2d 08:00" }],
  ["until: calendar pattern", { until: "*-*-01 08:00", tz: "Europe/Prague" }],
  ["until: weekday pattern", { until: "mon 09:00" }],
  ["until: RFC 3339", { until: "2026-09-01T08:00:00+02:00" }],
  ["until: RFC 9557 with IANA annotation", { until: "2026-09-01T08:00:00+02:00[Europe/Prague]" }],
  ["until: relaxed date-time", { until: "2026-09-01 08:00" }],
  ["until: bare number is unix milliseconds", { until: 1789000000000 }],
  ["until: $: expression", { until: "$: input.due_ms" }],
];

for (const [name, action] of accepted) {
  test(`PUT /definitions — accepts ${name}`, async () => {
    const { error } = await client.PUT("/definitions", { body: delayDef(action) as never });
    expect(error).toBeUndefined();
  });
}

const rejected: Array<[string, Record<string, unknown>]> = [
  // A quoted number has no unit, and "5000" could mean 5s or 5000ms. The bare number
  // form exists precisely so this stays unambiguous.
  ["for: unitless string", { for: "5000" }],
  ["for: unknown unit", { for: "2x" }],
  ["for: empty string", { for: "" }],
  // An interpolation produces a string at runtime — the failure mode this syntax removes.
  ["for: ${ } interpolation", { for: "${ input.hours }h" }],
  ["until: ${ } interpolation", { until: "${ input.due_ms }" }],
  // Natural language is deliberately unsupported: definitions are stored and replayed, so
  // a locale-dependent parser would let an upgrade change what old rows mean.
  ["until: natural language", { until: "in two days" }],
  ["until: impossible calendar date", { until: "*-02-30 08:00" }],
  ["until: pattern without a clock", { until: "*-*-01" }],
  // Arity: exactly one slot.
  ["neither slot", {}],
  ["both slots", { for: "1h", until: "+1d 08:00" }],
  // An abbreviation means the wrong thing for half the year and resolves per host.
  ["abbreviated tz", { for: "1d", tz: "CET" }],
  ["unknown tz", { for: "1d", tz: "Europe/Nowhere" }],
];

for (const [name, action] of rejected) {
  test(`PUT /definitions — rejects ${name}`, async () => {
    const { data, error } = await client.PUT("/definitions", { body: delayDef(action) as never });
    expect(error).toBeDefined();
    expect(data).toBeUndefined();
  });
}

// A malformed duration must fail when the definition is applied, not three days into a
// run — so the message has to name the slot and both readings of the ambiguous value.
test("PUT /definitions — the unitless rejection names both readings", async () => {
  const { error } = await client.PUT("/definitions", {
    body: delayDef({ for: "5000" }) as never,
  });
  const msg = JSON.stringify(error);
  expect(msg).toContain("5000ms");
  expect(msg).toContain("5000s");
});
