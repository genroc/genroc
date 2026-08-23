// HTTP surface for the evaluator. The ONLY thing here that genroc reads is the status
// code: it is the retryability class, because on_error matches `http.NNN` and nothing
// finer. Everything diagnostic goes in the body, where a switch can branch on it.
//
// See README.md for the contract.

import { createServer, type IncomingMessage, type ServerResponse } from "node:http";

import { evaluate, type EvalRequest, type EvalFailure } from "./eval.ts";

const PORT = Number(process.env.PORT ?? 3010);
// Loopback by default because /eval is unauthenticated arbitrary code execution with this
// process's full filesystem, network and environment (README §"What this is not"). listen()
// with no host binds every interface, so dropping `HOST` publishes that.
const HOST = process.env.HOST ?? "127.0.0.1";
const MAX_BODY_BYTES = 4 << 20;

const JSON_HEADERS = { "content-type": "application/json" };

function send(res: ServerResponse, status: number, body: unknown): void {
  res.writeHead(status, JSON_HEADERS);
  res.end(typeof body === "string" ? body : JSON.stringify(body));
}

/** 422 is the whole permanent-fault class: a retry re-runs the same code on the same input
 *  and fails identically, whether it failed to compile, threw, timed out or would not
 *  serialise. Splitting it across statuses would only invite an on_error rule that retries. */
function failure(res: ServerResponse, f: EvalFailure): void {
  send(res, 422, f);
}

function badRequest(res: ServerResponse, message: string): void {
  send(res, 400, { kind: "bad_request", name: "BadRequest", message });
}

class BodyTooLarge extends Error {}

/** Refuses AT the cap rather than buffering past it and measuring afterwards: the bytes a
 *  cap exists to not accept must never be accepted. It stops READING but does not destroy
 *  the socket — a reset would reach genroc as `http.disconnected`, the unknowable class that
 *  is never retried, where a 413 is an ordinary permanent fault. server.ts closes it after
 *  the response is flushed. */
function readBody(req: IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let size = 0;
    req.on("data", (chunk: Buffer) => {
      size += chunk.length;
      if (size > MAX_BODY_BYTES) {
        req.pause();
        reject(new BodyTooLarge());
        return;
      }
      chunks.push(chunk);
    });
    req.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
    req.on("error", reject);
  });
}

async function handleEval(req: IncomingMessage, res: ServerResponse): Promise<void> {
  let raw: string;
  try {
    raw = await readBody(req);
  } catch (err) {
    if (err instanceof BodyTooLarge) {
      // The rest of the upload is never read, so the connection cannot be reused: answer,
      // flush, and only then drop it.
      res.setHeader("connection", "close");
      res.on("finish", () => req.destroy());
      send(res, 413, { kind: "bad_request", name: "PayloadTooLarge", message: `request body exceeds ${MAX_BODY_BYTES} bytes` });
      return;
    }
    throw err;
  }

  let body: unknown;
  try {
    body = JSON.parse(raw);
  } catch {
    return badRequest(res, "request body is not valid JSON");
  }
  if (typeof body !== "object" || body === null) return badRequest(res, "request body must be an object");

  const r = body as Partial<EvalRequest>;
  if (typeof r.code !== "string") return badRequest(res, "`code` is required and must be a string");

  const result = await evaluate({ ...r, code: r.code });
  if (!result.ok) return failure(res, result.failure);

  // The script's return value IS the body, so `responses: {200: T}` types self.result as
  // exactly T. An empty body is how genroc spells null — the right reading of `return;`.
  send(res, 200, result.body);
}

const server = createServer((req, res) => {
  const pathname = new URL(req.url ?? "/", `http://${req.headers.host ?? "localhost"}`).pathname;
  if (req.method === "GET" && pathname === "/health") {
    return send(res, 200, { ok: true });
  }
  if (req.method !== "POST" || pathname !== "/eval") {
    return send(res, 404, { kind: "not_found", message: `no route for ${req.method} ${pathname}` });
  }
  handleEval(req, res).catch((err: unknown) => {
    // Reaching here means the RUNNER broke, not the script — the one retryable class.
    const message = err instanceof Error ? err.message : String(err);
    send(res, 500, { kind: "internal", name: "EvaluatorFault", message });
  });
});

// Never hang up on an idle connection. Node's 5s default is shorter than a caller polling
// every 10s, so every tick was a coin flip on whether the close raced the next request into
// a reset the client cannot retry; letting the caller's pool close first cannot race.
server.keepAliveTimeout = 0;
// The script's budget is the only clock over an evaluation — a request may legitimately
// outlast Node's 300s default, and being cut by the server would report as `http.timeout`,
// which is unknowable and so never retried.
server.requestTimeout = 0;

// The ASSIGNED port, not the requested one: PORT=0 means "any free port", and a caller
// (the tests do this) reads the port back off this line.
server.listen(PORT, HOST, () => {
  const address = server.address();
  const port = typeof address === "object" && address !== null ? address.port : PORT;
  console.log(`script runner listening on http://${HOST}:${port}`);
});
