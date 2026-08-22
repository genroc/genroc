// The evaluation realm. One Worker per execution: a fresh global object per script, and a
// thread the host can kill mid-loop — the only thing that bounds a synchronous busy loop.
// eval.ts owns the budget and does the killing; nothing here knows about time.
//
// Everything that touches the script's VALUE lives on this side of the boundary — compiling,
// classifying, serialising — because this is the only realm the value exists in.

import { createRequire } from "node:module";

import type { EvalFailure, FailureKind, WorkerReply, WorkerRequest } from "./eval.ts";

const AsyncFunction = async function () {}.constructor as new (
  ...args: string[]
) => (...args: unknown[]) => Promise<unknown>;

const STRICT = '"use strict";\n';

// Bundled `node:*` imports survive as `require` calls — Bun externalises builtins and inlines
// everything else — and a function built by the AsyncFunction constructor has no `require` in
// scope. Passing one in is what makes an import of a builtin work at runtime rather than at
// typecheck only. Resolution is anchored here, which is right: only builtins reach it.
const scriptRequire = createRequire(import.meta.url);
const STACK_BYTES = 2_048;

/**
 * Line offset the AsyncFunction preamble adds, measured rather than assumed: the generated
 * wrapper's shape is engine-specific, and a hardcoded number silently misreports every
 * script's error location the day it changes.
 */
const lineOffset: Promise<number> = (async () => {
  // Same parameter list as a real compile: the preamble is what is being measured.
  const probe = new AsyncFunction("input", "require", STRICT + "throw new Error('probe');");
  try {
    await probe();
    return 0;
  } catch (err) {
    return reportedLine(err) - 1;
  }
})();

// Bun attributes a dynamically-compiled function's frames to the file that CALLED the
// constructor, so the path in them is worker.ts and only the line number is the script's.
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

async function run(req: WorkerRequest): Promise<WorkerReply> {
  const offset = await lineOffset;

  let fn: (...args: unknown[]) => Promise<unknown>;
  try {
    // No compile cache: the realm is discarded after this execution, so a cache in it could
    // never be hit. Repeated compilation is the price of the fresh global object.
    fn = new AsyncFunction("input", "require", STRICT + req.code);
  } catch (err) {
    return { ok: false, failure: describe(err, "compile_error", offset) };
  }

  let value: unknown;
  try {
    value = await fn(req.input, scriptRequire);
  } catch (err) {
    return { ok: false, failure: describe(err, "threw", offset) };
  }

  try {
    // undefined stringifies to undefined, not "undefined"; an empty body is how genroc
    // spells null, which is the right reading of a script that returned nothing.
    return { ok: true, body: value === undefined ? "" : JSON.stringify(value) ?? "" };
  } catch (err) {
    return { ok: false, failure: describe(err, "nonserializable", offset) };
  }
}

declare const self: { onmessage: ((event: MessageEvent) => void) | null };
declare function postMessage(message: unknown): void;

self.onmessage = async (event: MessageEvent) => {
  postMessage(await run(event.data as WorkerRequest));
};
