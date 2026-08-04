import { client } from "./client.ts";

// Shared fixtures for the per-entity genctl suites (tests/cli/*_test.ts). Each of those
// files owns one entity's commands and pins its full flag surface and displayed fields;
// everything they have in common lives here so a definition shape is described once.

/** A name unique per test, so nothing collides on the server the CLI suites share. */
export function uid(prefix: string): string {
  return `${prefix}_${crypto.randomUUID().replace(/-/g, "").slice(0, 8)}`;
}

/** Pull the instance id out of a `started: <id>  proc@vN  (status)` line. */
export function startedID(stdout: string): string {
  const m = stdout.match(/started:\s+(\S+)/);
  if (!m) throw new Error(`no started id in: ${stdout}`);
  return m[1];
}

/**
 * Today's date as genctl renders it. genctl works in the local zone, so a UTC-derived
 * date is off by one for part of every day — build the expectation the same way it does.
 */
export function localDate(at = new Date()): string {
  const p = (n: number) => String(n).padStart(2, "0");
  return `${at.getFullYear()}-${p(at.getMonth() + 1)}-${p(at.getDate())}`;
}

/**
 * A local "YYYY-MM-DD HH:MM:SS" stamp, the form --since/--until parse. Second resolution,
 * matching the flags' own precision.
 */
export function localStamp(at = new Date()): string {
  const p = (n: number) => String(n).padStart(2, "0");
  return `${localDate(at)} ${p(at.getHours())}:${p(at.getMinutes())}:${p(at.getSeconds())}`;
}

/**
 * Freeze the population a list read sees, so two reads compare like for like on a server
 * the other suites are writing to. Waits past a second boundary, then returns a stamp to
 * pass as --until: every row created from that second onward is excluded from every read
 * using it, whenever those reads happen.
 */
export async function frozenUntil(): Promise<string> {
  await new Promise((r) => setTimeout(r, 1100));
  return localStamp();
}

/** An id no instance will ever have, for the not-found paths. */
export const missingID = "00000000-0000-0000-0000-000000000000";

/** The per-list row cap (cmd/genctl/commands.go listCap); logs uses logTailDefault. */
export const listCap = 20;

// ── definition fixtures ─────────────────────────────────────────────────────────

/** Completes immediately: one switch task straight to end. */
export function switchDef(name: string) {
  return { name, tasks: [{ id: "s1", switch: [{ goto: "end" }] }] };
}

/** Requires {count} on input, so schema rejections and context contents are testable. */
export function inputDef(name: string) {
  return {
    name,
    input_schema: {
      type: "object",
      properties: { count: { type: "number" }, name: { type: "string" } },
      required: ["count"],
    },
    tasks: [{ id: "s1", switch: [{ goto: "end" }] }],
  };
}

/** Calls out over HTTP; the endpoint need not exist unless the test waits on it. */
export function restDef(name: string, endpoint = "http://localhost/x") {
  return {
    name,
    tasks: [{ id: "s1", action: { type: "fetch", url: endpoint }, switch: [{ goto: "end" }] }],
  };
}

/** Spawns childName, so dependency-aware behaviour (promote, status, --recursive) has a tree. */
export function childDef(name: string, childName: string) {
  return {
    name,
    tasks: [
      {
        id: "spawn",
        action: { type: "child_map", children: { out: { name: childName } } },
        switch: [{ goto: "end" }],
      },
    ],
  };
}

/**
 * Parks on an external action awaiting {approved: boolean}, so it sits `running external`
 * until a caller resolves or signals it — a stable non-terminal state.
 */
export function externalDef(name: string) {
  return {
    name,
    tasks: [
      {
        id: "approval",
        action: {
          type: "external",
          input: { msg: "approve me" },
          result_schema: {
            type: "object",
            properties: { approved: { type: "boolean" } },
            required: ["approved"],
          },
        },
        output: "$: self.result",
        switch: [{ goto: "end" }],
      },
    ],
  };
}

/**
 * Calls an unreachable endpoint with no on_error rules, so it fails on the first attempt.
 * Its trail spans debug/info/error, which is what makes --level worth filtering.
 */
export function failingDef(name: string) {
  return {
    name,
    tasks: [
      {
        id: "call",
        action: { type: "fetch", url: "http://127.0.0.1:1/x" },
        timeout: 1000,
        switch: [{ goto: "end" }],
      },
    ],
  };
}

/**
 * Ends in an authored raise, so --error-code has an exact value to match. The raise is the
 * switch's unconditional last case — a trailing `case` is rejected as a non-catch-all.
 * An unhandled raise settles the instance `raised`, not `failed`.
 */
export function raisingDef(name: string, code: string) {
  return {
    name,
    tasks: [{ id: "boom", switch: [{ raise: { code, message: `raised ${code}` } }] }],
  };
}

/**
 * Past the 8 KiB inline threshold, so a value carrying it is externalized to the object
 * store and shows as a {ref, size} reference until --resolve fetches it.
 */
export const BIG_BLOB = "B".repeat(20 * 1024);

/** Takes a {blob} input, the vehicle for BIG_BLOB. */
export function blobInputDef(name: string) {
  return {
    name,
    input_schema: {
      type: "object",
      properties: { blob: { type: "string" } },
      required: ["blob"],
    },
    tasks: [{ id: "s1", switch: [{ goto: "end" }] }],
  };
}

// ── waits ───────────────────────────────────────────────────────────────────────

/**
 * Poll the external-task queue until the instance's entry is armed, returning its resolve
 * token (`<instance-id>.<nonce>`). The shared server auto-polls, so parking lands a cycle
 * or two after run.
 */
export async function waitForExternalToken(id: string, timeoutMs = 5000): Promise<string> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const { data, error } = await client.GET("/external-tasks", {});
    if (error) throw new Error(`list external tasks failed: ${JSON.stringify(error)}`);
    const entry = (data?.items ?? []).find(
      (t) => typeof t.token === "string" && t.token.startsWith(`${id}.`),
    );
    if (entry?.token) return entry.token;
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`external task for ${id} was not queued within ${timeoutMs}ms`);
}
