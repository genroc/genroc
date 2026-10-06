// The evaluation realm: one Worker per execution, killable mid-loop by eval.ts, which owns the
// budget. Everything touching the script's VALUE lives here, the only realm it exists in.

import { registerHooks } from "node:module";
import { parentPort } from "node:worker_threads";

import type { EvalFailure, FailureKind, WorkerReply, WorkerRequest } from "./eval.ts";

// IMPORTED under our own URL rather than compiled from a string, so frames carry the author's
// line numbers and builtin imports resolve normally.
const SCRIPT_URL = "script:main";
const STACK_BYTES = 2_048;

// The only channel a load hook has to the source. Written per request and read once — a realm
// evaluates one script and is then discarded, so no second execution can observe it.
let source = "";

registerHooks({
  resolve: (specifier, context, next) =>
    specifier === SCRIPT_URL ? { url: SCRIPT_URL, shortCircuit: true } : next(specifier, context),
  load: (url, context, next) =>
    url === SCRIPT_URL ? { format: "module", source, shortCircuit: true } : next(url, context),
});

/** Keeps the frames that are the script's own. Everything below the last of them is runner
 *  plumbing the author cannot act on, and V8 puts this file's path in it. */
function scriptStack(err: unknown): string | undefined {
  if (!(err instanceof Error) || typeof err.stack !== "string") return undefined;
  const lines = err.stack.split("\n");
  let last = 0;
  for (let i = 0; i < lines.length; i++) if (lines[i]!.includes(SCRIPT_URL)) last = i;
  const stack = lines.slice(0, last + 1).join("\n");
  return stack.length > STACK_BYTES ? stack.slice(0, STACK_BYTES) : stack;
}

function describe(err: unknown, kind: FailureKind): EvalFailure {
  if (err instanceof Error) {
    // A subclass inherits `name` from Error.prototype, so `class LimitExceeded extends Error {}`
    // would report "Error"; a name the script set still wins.
    const name = err.name === "Error" && err.constructor.name ? err.constructor.name : err.name;
    return { kind, name, message: err.message, stack: scriptStack(err) };
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

/** Unparseable code or an unresolvable import is broken code; anything else the import throws is
 *  the module's top level running, i.e. the script throwing. */
function unloadable(err: unknown): boolean {
  const code = (err as { code?: unknown } | null)?.code;
  return err instanceof SyntaxError || (typeof code === "string" && code.startsWith("ERR_MODULE"));
}

async function run(req: WorkerRequest): Promise<WorkerReply> {
  source = req.code;

  let main: unknown;
  try {
    main = (await import(SCRIPT_URL)).default;
  } catch (err) {
    return { ok: false, failure: describe(err, unloadable(err) ? "compile_error" : "threw") };
  }
  if (typeof main !== "function") {
    const got = main === undefined ? "no default export" : `a ${typeof main}`;
    return {
      ok: false,
      failure: {
        kind: "compile_error",
        name: "NoDefaultExport",
        message: `a script must export default a function; this one has ${got}`,
      },
    };
  }

  let value: unknown;
  try {
    value = await (main as (input: unknown) => unknown)(req.input);
  } catch (err) {
    return { ok: false, failure: describe(err, "threw") };
  }

  try {
    // undefined stringifies to undefined, not "undefined"; an empty body is how genroc
    // spells null, which is the right reading of a script that returned nothing.
    return { ok: true, body: value === undefined ? "" : JSON.stringify(value) ?? "" };
  } catch (err) {
    return { ok: false, failure: describe(err, "nonserializable") };
  }
}

// eval.ts terminates this thread the moment the reply lands, while stdio may still be in the
// pipe. An empty write's callback fires once the queue drains, making the reply a barrier.
function flush(stream: NodeJS.WriteStream): Promise<void> {
  return new Promise((resolve) => stream.write("", () => resolve()));
}

// Non-null because this module only ever runs as a worker entry point; a null port here
// would mean eval.ts loaded it as a plain module, which nothing does.
parentPort!.on("message", async (req: WorkerRequest) => {
  const reply = await run(req);
  await Promise.all([flush(process.stdout), flush(process.stderr)]);
  parentPort!.postMessage(reply);
});
