import createClient from "openapi-fetch";
import { createServer } from "http";
import type { AddressInfo } from "net";
import type { components, paths } from "../generated/api.ts";
import { BASE_URL } from "./constants.ts";

// The spec declares the API prefix in `servers`; openapi-fetch does not read that, so the
// base URL carries it. `/healthz` is mounted at the ROOT (actionDef.Root) and is the one
// path this client cannot reach — rootClient is for those.
export const API_BASE = `${BASE_URL}/api`;
export const client = createClient<paths>({ baseUrl: API_BASE });
export const rootClient = createClient<paths>({ baseUrl: BASE_URL });
export const createClientTyped: typeof createClient<paths> = (options) =>
  createClient<paths>(options);

type ApiClient = Pick<typeof client, "GET">;
type PostClient = Pick<typeof client, "POST">;

type InstanceQuery = NonNullable<paths["/instances"]["get"]["parameters"]["query"]>;

// listAllInstances pages forward through GET /instances, following page.after
// until it is absent, and returns every matching instance. List endpoints now cap
// a page (default/cap 1000), so callers that need the whole set must page rather
// than read a single response.
/**
 * Every instance, CHILDREN INCLUDED — the endpoint lists roots only by default (one row
 * per tree), so enumerating a tree from the outside has to ask for them.
 */
export async function listAllInstances(
  apiClient: ApiClient = client,
  query: Pick<InstanceQuery, "status"> = {},
): Promise<components["schemas"]["ApiInstanceSummaryResp"][]> {
  const all: components["schemas"]["ApiInstanceSummaryResp"][] = [];
  let after: string | undefined;
  for (;;) {
    const { data, error } = await apiClient.GET("/instances", {
      params: { query: { ...query, children: true, after, limit: 1000 } },
    });
    if (error) throw new Error(`list instances failed: ${JSON.stringify(error)}`);
    all.push(...(data?.items ?? []));
    after = data?.page.after || undefined;
    if (!after) return all;
  }
}

/**
 * The instance's STATE: everything stored on it, bookkeeping slots included. `context` on the
 * status response carries only what a definition's author reads (input/outputs/output/error);
 * the engine's own slots -- _external, _spawn_*, _error_data -- live
 * here, because they are state and not context.
 */
export async function instanceState(
  id: string,
  apiClient: typeof client = client,
): Promise<Record<string, unknown>> {
  const { data, error } = await apiClient.GET("/instances/{id}/detail", {
    params: { path: { id } },
  });
  if (error) throw new Error(`detail ${id}: ${JSON.stringify(error)}`);
  return (data!.state ?? {}) as Record<string, unknown>;
}

/**
 * The children a spawn task made, keyed the way its action type keys them. Derived by the
 * server from the child rows, not read off a slot on the parent — so a `child_list` that
 * spawned nothing names no task at all.
 */
export async function childrenOfTask(
  id: string,
  taskID: string,
  apiClient: typeof client = client,
): Promise<unknown> {
  const { data, error } = await apiClient.GET("/instances/{id}/detail", {
    params: { path: { id } },
  });
  if (error) throw new Error(`detail ${id}: ${JSON.stringify(error)}`);
  return (data!.children as Record<string, unknown> | undefined)?.[taskID];
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
  // The state at the deadline, in the message: a timeout says only that the instance did not
  // settle, and the three answers to WHY are distinguishable from the row. `wait_state:external`
  // means an answer never un-parked it; a moving `updated_at` means the engine is advancing it
  // and something else is slow; a still one means it was never claimed.
  const { data } = await apiClient.GET("/instances/{id}/detail", { params: { path: { id } } });
  const at = data as { status?: string; wait_state?: string; task?: string; updated_at?: string };
  throw new Error(
    `instance ${id} did not complete within ${timeoutMs}ms ` +
      `(status ${at?.status ?? "?"}, wait_state ${at?.wait_state || "-"}, ` +
      `task ${at?.task ?? "?"}, updated_at ${at?.updated_at ?? "?"})`,
  );
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

/** One entry of a response's `objects` section: a value too large to carry inline. */
export type ObjectEntry = { path: (string | number)[]; ref: string; size: number };

/** Fetch one externalized value by its content hash. */
export async function fetchObject(ref: string, apiClient: PostClient | typeof client = client): Promise<string> {
  const { data, error } = await (apiClient as typeof client).GET("/objects/{ref}", {
    params: { path: { ref } },
  });
  if (error) throw new Error(`fetch object ${ref} failed: ${JSON.stringify(error)}`);
  return (data as { data: string }).data;
}

/**
 * spliceObjects is what every recipient of the objects protocol owes it: fetch each listed value
 * and put it back at the path it named. The server no longer does this — `?resolve=true`
 * materialized every slot behind one query parameter, which is an unbounded response nobody
 * asked the size of. Paths are arrays of keys, so walking one needs no parser and no unescaping.
 *
 * A section belongs to whatever object owns its values, so this splices the body's OWN section
 * and then each entry's, recursing into `items`. That is what makes a path stable in a list:
 * it is rooted at the entry, so accumulating pages or reversing rows cannot invalidate it.
 */
export async function spliceObjects<T>(body: T, apiClient: typeof client = client): Promise<T> {
  const owner = body as { objects?: ObjectEntry[]; items?: unknown[] };
  for (const e of owner.objects ?? []) {
    const raw = await fetchObject(e.ref, apiClient);
    let value: unknown;
    try {
      value = JSON.parse(raw);
    } catch {
      value = raw; // a raw (non-JSON) log payload goes back as the string it is
    }
    let cur: any = body;
    for (let i = 0; i < e.path.length - 1; i++) cur = cur?.[e.path[i]];
    if (cur) cur[e.path[e.path.length - 1]] = value;
  }
  for (const item of owner.items ?? []) await spliceObjects(item, apiClient);
  return body;
}

/** The entry covering `path`, or undefined when that slot was carried inline. */
export function objectAt(body: unknown, path: (string | number)[]): ObjectEntry | undefined {
  const entries = ((body as { objects?: ObjectEntry[] }).objects ?? []) as ObjectEntry[];
  return entries.find((e) => e.path.length === path.length && e.path.every((p, i) => p === path[i]));
}
