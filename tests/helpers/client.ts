import createClient from "openapi-fetch";
import { createServer } from "http";
import type { AddressInfo } from "net";
import type { components, paths } from "../generated/api.ts";
import { BASE_URL } from "./constants.ts";

export const client = createClient<paths>({ baseUrl: BASE_URL });
export const createClientTyped: typeof createClient<paths> = (options) =>
  createClient<paths>(options);

type ApiClient = Pick<typeof client, "GET">;
type PostClient = Pick<typeof client, "POST">;

type InstanceQuery = NonNullable<paths["/instances"]["get"]["parameters"]["query"]>;

// listAllInstances pages forward through GET /instances, following page.after
// until it is absent, and returns every matching instance. List endpoints now cap
// a page (default/cap 1000), so callers that need the whole set must page rather
// than read a single response.
export async function listAllInstances(
  apiClient: ApiClient = client,
  query: Pick<InstanceQuery, "status"> = {},
): Promise<components["schemas"]["ApiInstanceSummaryResp"][]> {
  const all: components["schemas"]["ApiInstanceSummaryResp"][] = [];
  let after: string | undefined;
  for (;;) {
    const { data, error } = await apiClient.GET("/instances", {
      params: { query: { ...query, after, limit: 1000 } },
    });
    if (error) throw new Error(`list instances failed: ${JSON.stringify(error)}`);
    all.push(...(data?.items ?? []));
    after = data?.page.after || undefined;
    if (!after) return all;
  }
}

export async function waitForInstance(
  id: string,
  timeoutMs = 5000,
  apiClient: ApiClient = client,
): Promise<string> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const { data, error } = await apiClient.GET("/instances/{id}", {
      params: { path: { id } },
    });
    if (error) throw new Error(`get_instance failed: ${JSON.stringify(error)}`);
    const status = data?.status;
    // paused is deliberately absent: it is not an outcome, just work that is not
    // being advanced, so waiting for a terminal state must not stop on it.
    // raised is present: a `raise` clause is a settled conclusion like the other two.
    if (status === "completed" || status === "failed" || status === "raised")
      return status!;
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`instance ${id} did not complete within ${timeoutMs}ms`);
}

// Trigger one engine poll cycle. Returns the number of instances processed.
// Only useful when the server was started with --poll 0 (manual tick mode).
// advanceMs shifts the server clock forward (milliseconds) before the tick,
// expiring leases and retry timers without real waits.
export async function tick(
  apiClient: PostClient = client,
  advanceMs?: number,
): Promise<number> {
  const { data, error } = await apiClient.POST("/tick", {
    body: advanceMs ? { advance_ms: advanceMs } : undefined,
  });
  if (error) throw new Error(`tick failed: ${JSON.stringify(error)}`);
  return (data as { count: number }).count;
}

interface MockServiceOptions {
  // The JSON body sent for every response. Defaults to {}.
  response?: Record<string, unknown>;
  // HTTP status code to return. Defaults to 200.
  statusCode?: number;
  // How long to delay the very first request before responding.
  // 0 (default) = respond immediately.
  // Infinity     = never respond; use this to simulate a worker hanging mid-task.
  firstRequestDelayMs?: number;
}

export async function startMockService(port: number, options: MockServiceOptions = {}) {
  const { response = {}, statusCode = 200, firstRequestDelayMs = 0 } = options;
  const body = JSON.stringify(response);

  let count = 0;
  // The request line as the server received it, so a test can assert what was actually sent
  // — query encoding is only observable here.
  const urls: string[] = [];
  let resolveFirst!: () => void;
  const firstRequestReceived = new Promise<void>((r) => {
    resolveFirst = r;
  });
  // pendingSend is set when firstRequestDelayMs === Infinity so the caller
  // can unblock the held HTTP response by calling release().
  let pendingSend: (() => void) | undefined;

  const server = createServer((req, res) => {
    count++;
    urls.push(req.url ?? "");
    req.socket.on("error", () => {}); // suppress ECONNRESET
    res.on("error", () => {});

    const send = () => {
      res.writeHead(statusCode, { "Content-Type": "application/json" });
      res.end(body);
    };

    if (count === 1) {
      resolveFirst();
      if (!isFinite(firstRequestDelayMs)) {
        // Hold until release() is called.
        pendingSend = send;
      } else if (firstRequestDelayMs > 0) {
        setTimeout(send, firstRequestDelayMs);
      } else {
        send();
      }
    } else {
      send();
    }
  });
  server.on("clientError", () => {});
  await new Promise<void>((r) => server.listen(port, r));
  const boundPort = (server.address() as AddressInfo).port;

  return {
    port: boundPort,
    firstRequestReceived,
    requestCount: () => count,
    requestUrls: () => [...urls],
    // Unblocks the held first request when firstRequestDelayMs === Infinity.
    release: () => { pendingSend?.(); pendingSend = undefined; },
    stop: () => new Promise<void>((r) => server.close(() => r())),
  };
}
