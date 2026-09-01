// Evaluation core: run a code string in its OWN realm and classify every outcome into one of
// the failure kinds in README.md. Nothing here knows about genroc — worker.ts is the only
// thing that talks to the queue — so this stays testable and the containment stays swappable.
//
// The containment is a Worker per execution (realm.ts). It is what makes the budget real:
// a synchronous busy loop never yields, so no in-process timer can interrupt it, and only a
// thread the host can kill bounds it.

import { Worker } from "node:worker_threads";

export type EvalRequest = {
  code: string;
  input?: unknown;
  timeout_ms?: number;
};

/** Every kind here is PERMANENT: a retry re-runs the same code on the same input and fails
 *  identically. The retryable class has no kind because it is not an outcome — a runner that
 *  faults releases its claim and lets another worker take the task. */
export type FailureKind = "compile_error" | "threw" | "timeout" | "nonserializable" | "exited";

export type EvalFailure = {
  kind: FailureKind;
  name: string;
  message: string;
  stack?: string;
};

/** `body` is JSON TEXT, not a value: serialising in the realm is what makes a nonserializable
 *  return a script fault rather than a 500 thrown out of the response path. It is also what
 *  crosses the worker boundary — structured clone would refuse a different set of values. */
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

/** Thrown, not returned: a realm that fails to start is the RUNNER faulting, which worker.ts
 *  answers by releasing the claim rather than by reporting an outcome. A script fault is a
 *  return value. */
class RealmFault extends Error {
  constructor(message: string) {
    super(message);
    this.name = "RealmFault";
  }
}

export async function evaluate(req: EvalRequest): Promise<EvalResult> {
  const budget = typeof req.timeout_ms === "number" ? req.timeout_ms : DEFAULT_TIMEOUT_MS;

  const worker = new Worker(REALM_URL);
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await new Promise<EvalResult>((resolve, reject) => {
      timer = setTimeout(() => resolve(timedOut(budget)), budget);
      worker.once("message", (reply: WorkerReply) => resolve(reply));
      // A script may end its own realm (`process.exit()`), which is not a throw and would
      // otherwise present as a hang until the budget expired. Our own terminate() raises
      // this too, by which time the promise has settled and the first result stands.
      worker.once("exit", (code: number) => resolve(exited(code)));
      worker.once("error", (err: Error) => reject(new RealmFault(errorText(err))));
      worker.postMessage({ code: req.code, input: req.input } satisfies WorkerRequest);
    });
  } finally {
    clearTimeout(timer);
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
