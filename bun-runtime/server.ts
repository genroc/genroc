// HTTP surface for the evaluator. The ONLY thing here that genroc reads is the status
// code: it is the retryability class, because on_error matches `http.NNN` and nothing
// finer. Everything diagnostic goes in the body, where a switch can branch on it.
//
// See README.md for the contract.

import { evaluate, type EvalRequest, type EvalFailure } from "./eval.ts";

const PORT = Number(process.env.PORT ?? 3010);
const MAX_BODY_BYTES = 4 << 20;

const JSON_HEADERS = { "content-type": "application/json" };

/** 422 is the whole permanent-fault class: a retry re-runs the same code on the same input
 *  and fails identically, whether it failed to compile, threw, timed out or would not
 *  serialise. Splitting it across statuses would only invite an on_error rule that retries. */
function failure(f: EvalFailure): Response {
  return new Response(JSON.stringify(f), { status: 422, headers: JSON_HEADERS });
}

function badRequest(message: string): Response {
  return new Response(JSON.stringify({ kind: "bad_request", name: "BadRequest", message }), {
    status: 400,
    headers: JSON_HEADERS,
  });
}

async function handleEval(req: Request): Promise<Response> {
  let body: unknown;
  try {
    body = await req.json();
  } catch {
    return badRequest("request body is not valid JSON");
  }
  if (typeof body !== "object" || body === null) return badRequest("request body must be an object");

  const r = body as Partial<EvalRequest>;
  if (typeof r.code !== "string") return badRequest("`code` is required and must be a string");

  const result = await evaluate({ ...r, code: r.code });
  if (!result.ok) return failure(result.failure);

  // The script's return value IS the body, so `responses: {200: T}` types self.result as
  // exactly T. An empty body is how genroc spells null — the right reading of `return;`.
  return new Response(result.body, { status: 200, headers: JSON_HEADERS });
}

const server = Bun.serve({
  port: PORT,
  maxRequestBodySize: MAX_BODY_BYTES,
  async fetch(req) {
    const { pathname } = new URL(req.url);
    if (req.method === "GET" && pathname === "/health") {
      return new Response(JSON.stringify({ ok: true }), { status: 200, headers: JSON_HEADERS });
    }
    if (req.method !== "POST" || pathname !== "/eval") {
      return new Response(JSON.stringify({ kind: "not_found", message: `no route for ${req.method} ${pathname}` }), {
        status: 404,
        headers: JSON_HEADERS,
      });
    }
    try {
      return await handleEval(req);
    } catch (err) {
      // Reaching here means the RUNNER broke, not the script — the one retryable class.
      const message = err instanceof Error ? err.message : String(err);
      return new Response(JSON.stringify({ kind: "internal", name: "EvaluatorFault", message }), {
        status: 500,
        headers: JSON_HEADERS,
      });
    }
  },
});

console.log(`script runner listening on http://localhost:${server.port}`);
