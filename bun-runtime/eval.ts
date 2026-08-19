// Evaluation core: compile a code string, run it, and classify every outcome into one of
// the wire classes in README.md. No HTTP here — server.ts is the only thing that knows a
// status code, so this stays testable and swappable for a Worker/subprocess strategy.

export type EvalRequest = {
  code: string;
  input?: unknown;
  now?: number;
  seed?: string;
  timeout_ms?: number;
};

/** Permanent-vs-retryable is the STATUS's job; kind is the detail a switch branches on. */
export type FailureKind = "compile_error" | "threw" | "timeout" | "nonserializable";

export type EvalFailure = {
  kind: FailureKind;
  name: string;
  message: string;
  stack?: string;
};

/** `body` is JSON TEXT, not a value: serialising here is what makes a nonserializable
 *  return a script fault rather than a 500 thrown out of the response path. */
export type EvalResult =
  | { ok: true; body: string }
  | { ok: false; failure: EvalFailure };

const AsyncFunction = async function () {}.constructor as new (
  ...args: string[]
) => (...args: unknown[]) => Promise<unknown>;

const STRICT = '"use strict";\n';
const DEFAULT_TIMEOUT_MS = 5_000;
const STACK_BYTES = 2_048;
const CACHE_MAX = 256;

// Compiled bodies keyed by source text — content addressing by identity, so no
// invalidation. Insertion-ordered, evicted oldest-first: this is a bounded cache, not a
// registry, and a script's code arrives on every request anyway.
const cache = new Map<string, (...args: unknown[]) => Promise<unknown>>();

/**
 * Line offset the AsyncFunction preamble adds, measured rather than assumed: the generated
 * wrapper's shape is engine-specific, and a hardcoded number silently misreports every
 * script's error location the day it changes.
 */
const lineOffset: Promise<number> = (async () => {
  // Same parameter list as a real compile: the preamble is what is being measured.
  const probe = new AsyncFunction("input", "ctx", "Date", "Math", STRICT + "throw new Error('probe');");
  try {
    await probe();
    return 0;
  } catch (err) {
    return reportedLine(err) - 1;
  }
})();

// Bun attributes a dynamically-compiled function's frames to the file that CALLED the
// constructor, so the path in them is eval.ts and only the line number is the script's.
// The `anonymous` frame is therefore the boundary: everything above it is script code,
// everything below is runner plumbing.
const ANON_FRAME = /\bat anonymous\b/;
// `    at name (path:LINE:COL)` — name and the wrapping parens are both optional.
const FRAME = /^(\s*)at\s+(?:(.*?)\s+)?\(?(?:.*?):(\d+):(\d+)\)?$/;

/** Line number of the throw as the engine reported it, or 1 if the stack is unreadable. */
function reportedLine(err: unknown): number {
  const stack = err instanceof Error && typeof err.stack === "string" ? err.stack : "";
  const frame = stack.split("\n").find((l) => ANON_FRAME.test(l)) ?? "";
  const m = frame.match(FRAME);
  return m ? Number(m[3]) : 1;
}

function fnv1a(s: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return h >>> 0;
}

function mulberry32(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

// A Proxy forwards writes to its target, so without these a script assigning `Math.random`
// or `Date.now` patches the process-wide global and every LATER request reads the patch.
// Refusing is a TypeError under "use strict", which lands as an ordinary script fault.
const readOnly = {
  set: () => false,
  defineProperty: () => false,
  deleteProperty: () => false,
} as const;

/**
 * `Date` with its reading of "now" pinned, NOT deleted: a retry re-executes the script, so
 * an unpinned clock makes attempt two differ from attempt one — while deleting `Date`
 * would leave the generated types asserting what the runtime contradicts.
 * A Proxy over the real Date so `new Date(x)`, `Date.parse`, `Date.UTC` and `instanceof`
 * all keep working.
 */
function pinnedDate(now: number): DateConstructor {
  return new Proxy(Date, {
    ...readOnly,
    construct(target, args, newTarget) {
      if (args.length === 0) return Reflect.construct(target, [now], newTarget);
      return Reflect.construct(target, args, newTarget);
    },
    apply() {
      return new Date(now).toString();
    },
    get(target, prop, receiver) {
      if (prop === "now") return () => now;
      return Reflect.get(target, prop, receiver);
    },
  }) as DateConstructor;
}

function pinnedMath(seed: string): Math {
  const random = mulberry32(fnv1a(seed));
  return new Proxy(Math, {
    ...readOnly,
    get(target, prop, receiver) {
      if (prop === "random") return random;
      return Reflect.get(target, prop, receiver);
    },
  });
}

/** Renumbers each script frame to the line the AUTHOR wrote and drops the runner's own. */
function scriptStack(err: unknown, offset: number): string | undefined {
  if (!(err instanceof Error) || typeof err.stack !== "string") return undefined;
  const lines = err.stack.split("\n");
  const boundary = lines.findIndex((l) => ANON_FRAME.test(l));
  const frames = (boundary >= 0 ? lines.slice(0, boundary + 1) : lines.slice(0, 1))
    .map((line) => {
      const m = line.match(FRAME);
      if (!m) return line; // the `Error: message` header, kept as-is
      const [, indent, name, ln, col] = m;
      const at = name && name !== "anonymous" ? `at ${name} ` : "at ";
      return `${indent}${at}(script:${Math.max(1, Number(ln) - offset)}:${col})`;
    })
    .join("\n");
  return frames.length > STACK_BYTES ? frames.slice(0, STACK_BYTES) : frames;
}

function describe(err: unknown, kind: FailureKind, offset: number): EvalFailure {
  if (err instanceof Error) {
    return { kind, name: err.name, message: err.message, stack: scriptStack(err, offset) };
  }
  // A script may throw a non-Error (`throw {code: "x"}`), so name/message must not assume one.
  return { kind, name: "Thrown", message: safeText(err) };
}

function safeText(v: unknown): string {
  try {
    return typeof v === "string" ? v : JSON.stringify(v) ?? String(v);
  } catch {
    return String(v);
  }
}

function compile(code: string): (...args: unknown[]) => Promise<unknown> {
  const hit = cache.get(code);
  if (hit) return hit;
  const fn = new AsyncFunction("input", "ctx", "Date", "Math", STRICT + code);
  if (cache.size >= CACHE_MAX) {
    const oldest = cache.keys().next();
    if (!oldest.done) cache.delete(oldest.value);
  }
  cache.set(code, fn);
  return fn;
}

export async function evaluate(req: EvalRequest): Promise<EvalResult> {
  const offset = await lineOffset;
  const now = typeof req.now === "number" ? req.now : Date.now();
  const seed = req.seed ?? "";
  const budget = typeof req.timeout_ms === "number" ? req.timeout_ms : DEFAULT_TIMEOUT_MS;

  let fn: (...args: unknown[]) => Promise<unknown>;
  try {
    fn = compile(req.code);
  } catch (err) {
    return { ok: false, failure: describe(err, "compile_error", offset) };
  }

  let timer: ReturnType<typeof setTimeout> | undefined;
  let value: unknown;
  try {
    const ctx = { now, seed };
    // Only bounds an AWAIT that never settles. A synchronous busy loop never yields, so
    // this timer never fires — see README.md "What the timeout does not cover".
    const expired = new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new TimeoutError(budget)), budget);
    });
    value = await Promise.race([fn(req.input, ctx, pinnedDate(now), pinnedMath(seed)), expired]);
  } catch (err) {
    const kind: FailureKind = err instanceof TimeoutError ? "timeout" : "threw";
    return { ok: false, failure: describe(err, kind, offset) };
  } finally {
    clearTimeout(timer);
  }

  try {
    // undefined stringifies to undefined, not "undefined"; an empty body is how genroc
    // spells null, which is the right reading of a script that returned nothing.
    return { ok: true, body: value === undefined ? "" : JSON.stringify(value) ?? "" };
  } catch (err) {
    return { ok: false, failure: describe(err, "nonserializable", offset) };
  }
}

class TimeoutError extends Error {
  constructor(ms: number) {
    super(`script exceeded its ${ms}ms budget`);
    this.name = "TimeoutError";
  }
}
