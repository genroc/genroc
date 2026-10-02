// Imports a script module in its own realm (a Worker per execution, realm.ts), calls its default
// export, and classifies every outcome (README.md). Only a killable thread bounds a synchronous
// busy loop, which is what makes the budget real. Knows nothing of genroc; worker.ts does.

import { Worker } from "node:worker_threads";

export type EvalRequest = {
  /** An ES module whose default export is the function to run; `input` is its argument. */
  code: string;
  input?: unknown;
  timeout_ms?: number;
};

/** Every kind is PERMANENT: a retry fails identically. A faulting runner is not an outcome; it
 *  releases its claim. */
export type FailureKind = "compile_error" | "threw" | "timeout" | "nonserializable" | "exited";

export type EvalFailure = {
  kind: FailureKind;
  name: string;
  message: string;
  stack?: string;
};

/** JSON TEXT serialised in the realm, so a nonserializable return is a script fault, not a 500,
 *  and structured clone (which refuses a different set of values) never sees the value. */
export type EvalResult =
  | { ok: true; body: string }
  | { ok: false; failure: EvalFailure };

/** The message the host posts into the realm, and the only reply it accepts back. */
export type WorkerRequest = { code: string; input?: unknown };
export type WorkerReply = EvalResult;

const DEFAULT_TIMEOUT_MS = 5_000;
// Resolved from THIS file's own extension: run from a checkout it is `.ts`, and from the
// published package `.js`, because Node will not strip types under node_modules.
const REALM_URL = new URL(
  import.meta.url.endsWith(".ts") ? "./realm.ts" : "./realm.js",
  import.meta.url,
);

/** Thrown, not returned: a realm that fails to start is the runner faulting, which worker.ts
 *  answers by releasing the claim. */
class RealmFault extends Error {
  constructor(message: string) {
    super(message);
    this.name = "RealmFault";
  }
}

/** Thrown when `signal` aborts: the answer is no longer wanted, so worker.ts releases rather
 *  than answers. Not a RealmFault: nothing is wrong. */
export class Cancelled extends Error {
  constructor() {
    super("cancelled");
    this.name = "Cancelled";
  }
}

export async function evaluate(req: EvalRequest, signal?: AbortSignal): Promise<EvalResult> {
  const budget = typeof req.timeout_ms === "number" ? req.timeout_ms : DEFAULT_TIMEOUT_MS;

  const worker = new Worker(REALM_URL);
  let timer: ReturnType<typeof setTimeout> | undefined;
  let onAbort: (() => void) | undefined;
  try {
    return await new Promise<EvalResult>((resolve, reject) => {
      timer = setTimeout(() => resolve(timedOut(budget)), budget);
      // The abort is wired to the same promise as the budget, so both settle through the one
      // finally below -- which is what guarantees the thread is gone before either returns.
      if (signal) {
        if (signal.aborted) reject(new Cancelled());
        onAbort = () => reject(new Cancelled());
        signal.addEventListener("abort", onAbort, { once: true });
      }
      worker.once("message", (reply: WorkerReply) => resolve(reply));
      // A script may `process.exit()`, which would otherwise look like a hang until the budget.
      // Our own terminate() fires this too, after the promise has settled.
      worker.once("exit", (code: number) => resolve(exited(code)));
      worker.once("error", (err: Error) => reject(new RealmFault(errorText(err))));
      worker.postMessage({ code: req.code, input: req.input } satisfies WorkerRequest);
    });
  } finally {
    clearTimeout(timer);
    if (signal && onAbort) signal.removeEventListener("abort", onAbort);
    // Awaited, and the whole point: on the timeout path a thread is still burning a core, and
    // resolving before it is gone would report an evaluation the machine is still running.
    await worker.terminate();
  }
}

function timedOut(ms: number): EvalResult {
  return {
    ok: false,
    failure: { kind: "timeout", name: "TimeoutError", message: `script exceeded its ${ms}ms budget` },
  };
}

function exited(code: number): EvalResult {
  return {
    ok: false,
    failure: {
      kind: "exited",
      name: "RealmExited",
      message: `the script ended its own realm with code ${code} instead of returning`,
    },
  };
}

function errorText(e: unknown): string {
  const message = (e as { message?: unknown } | null)?.message;
  return typeof message === "string" && message !== "" ? message : "the evaluation realm failed to start";
}
