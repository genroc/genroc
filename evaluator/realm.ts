// The evaluation realm. One Worker per execution: a fresh global object per script, and a
// thread the host can kill mid-loop — the only thing that bounds a synchronous busy loop.
// eval.ts owns the budget and does the killing; nothing here knows about time.
//
// Everything that touches the script's VALUE lives on this side of the boundary — compiling,
// classifying, serialising — because this is the only realm the value exists in.

import { createRequire } from "node:module";
import { parentPort } from "node:worker_threads";

import type { EvalFailure, FailureKind, WorkerReply, WorkerRequest } from "./eval.ts";

const AsyncFunction = async function () {}.constructor as new (
  ...args: string[]
) => (...args: unknown[]) => Promise<unknown>;

const STRICT = '"use strict";\n';

// Bundled `node:*` imports survive as `require` calls — the importer externalises builtins and
// inlines everything else — and a function built by the AsyncFunction constructor has no
// `require` in scope. Passing one in is what makes an import of a builtin work at runtime rather than at
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

// V8 marks a frame compiled by the AsyncFunction constructor with the site that CALLED the
// constructor, then the script's OWN position:
//   at inner (eval at run (file:///…/worker.ts:107:10), <anonymous>:6:9)
// `eval at` is therefore what separates script frames from runner plumbing — matched without
// the function name, which is whatever encloses the `new AsyncFunction` below. The LAST such
// frame is the body's top level: frames interleave, since a script can throw inside a native
// callback.
const SCRIPT_FRAME = /\(eval at /;
// The trailing `<anonymous>:LINE:COL` — the script's position, after the host file's own.
const POSITION = /<anonymous>:(\d+):(\d+)\)?\s*$/;
// `    at name (` — absent on the top-level frame, which V8 names `eval`.
const FRAME_NAME = /^\s*at\s+(?:async\s+)?([^\s(]+)\s*\(/;

/** Line number of the throw as the engine reported it, or 1 if the stack is unreadable. */
function reportedLine(err: unknown): number {
  const stack = err instanceof Error && typeof err.stack === "string" ? err.stack : "";
  const frame = stack.split("\n").find((l) => SCRIPT_FRAME.test(l)) ?? "";
  const m = frame.match(POSITION);
  return m ? Number(m[1]) : 1;
}

/** Renumbers each script frame to the line the AUTHOR wrote and drops the runner's own.
 *  Rewriting the whole location is also what keeps the runner's path out of a script's
 *  stack — V8 puts it inside every compiled frame. */
function scriptStack(err: unknown, offset: number): string | undefined {
  if (!(err instanceof Error) || typeof err.stack !== "string") return undefined;
  const lines = err.stack.split("\n");
  let boundary = -1;
  for (let i = 0; i < lines.length; i++) if (SCRIPT_FRAME.test(lines[i]!)) boundary = i;
  const frames = (boundary >= 0 ? lines.slice(0, boundary + 1) : lines.slice(0, 1))
    .map((line) => {
      const pos = line.match(POSITION);
      if (!pos) return line; // the `Error: message` header and native frames, kept as-is
      const name = line.match(FRAME_NAME)?.[1];
      const at = name && name !== "eval" && name !== "anonymous" ? `at ${name} ` : "at ";
      const indent = line.match(/^\s*/)![0];
      return `${indent}${at}(script:${Math.max(1, Number(pos[1]) - offset)}:${pos[2]})`;
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

// Non-null because this module only ever runs as a worker entry point; a null port here
// would mean eval.ts loaded it as a plain module, which nothing does.
parentPort!.on("message", async (req: WorkerRequest) => {
  parentPort!.postMessage(await run(req));
});
