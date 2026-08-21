// Evaluation core: run a code string in its OWN realm and classify every outcome into one of
// the wire classes in README.md. No HTTP here — server.ts is the only thing that knows a
// status code, so this stays testable and the containment strategy stays swappable.
//
// The containment is a Worker per execution (worker.ts). It is what makes the budget real:
// a synchronous busy loop never yields, so no in-process timer can interrupt it, and only a
// thread the host can kill bounds it. Measured on Bun 1.3.14: `terminate()` stops a spinning
// worker, and a fresh realm costs ~1.7ms.

export type EvalRequest = {
  code: string;
  input?: unknown;
  now?: number;
  seed?: string;
  timeout_ms?: number;
};

/** Permanent-vs-retryable is the STATUS's job; kind is the detail a switch branches on. */
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
export type WorkerRequest = { code: string; input?: unknown; now: number; seed: string };
export type WorkerReply = EvalResult;

const DEFAULT_TIMEOUT_MS = 5_000;
const WORKER_URL = new URL("./worker.ts", import.meta.url).href;

/** Thrown, not returned: a realm that fails to start is the RUNNER faulting, and server.ts
 *  turns a thrown error into the one retryable status. A script fault is a return value. */
class RealmFault extends Error {
  constructor(message: string) {
    super(message);
    this.name = "RealmFault";
  }
}

export async function evaluate(req: EvalRequest): Promise<EvalResult> {
  const now = typeof req.now === "number" ? req.now : Date.now();
  const seed = req.seed ?? "";
  const budget = typeof req.timeout_ms === "number" ? req.timeout_ms : DEFAULT_TIMEOUT_MS;

  const worker = new Worker(WORKER_URL);
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await new Promise<EvalResult>((resolve, reject) => {
      timer = setTimeout(() => resolve(timedOut(budget)), budget);
      worker.addEventListener("message", (e) => resolve(e.data as WorkerReply), { once: true });
      // A script may end its own realm (`process.exit()`), which is not a throw and would
      // otherwise present as a hang until the budget expired.
      worker.addEventListener("close", (e) => resolve(exited(e.code)), { once: true });
      worker.addEventListener("error", (e) => reject(new RealmFault(errorText(e))), { once: true });
      worker.postMessage({ code: req.code, input: req.input, now, seed } satisfies WorkerRequest);
    });
  } finally {
    clearTimeout(timer);
    // Unconditional, and the whole point: on the timeout path a thread is still burning a
    // core, and a worker that outlives its evaluation is a leaked thread on every path.
    worker.terminate();
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
