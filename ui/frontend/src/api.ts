// The genroc client. Always same-origin (Vite proxies /api in dev), so no base URL and no CORS.
// A stored bearer token is the no-proxy case, where a person pastes one; behind genroc-ui
// nothing is sent, and the proxy attaches a JWT minted from the session cookie.
import createFetchClient, { type Middleware } from "openapi-fetch";
import createClient from "openapi-react-query";
import type { components, paths } from "./generated/api.ts";

const TOKEN_KEY = "genroc.token";

export function token(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) ?? "";
  } catch {
    return ""; // a private window with storage blocked still gets a usable, if forgetful, UI
  }
}

export function setToken(v: string) {
  try {
    if (v) localStorage.setItem(TOKEN_KEY, v);
    else localStorage.removeItem(TOKEN_KEY);
  } catch {
    /* ignore: the value stays in memory for this page's lifetime */
  }
}

/** Every failed call surfaces as this, whatever the endpoint: the generated types name the error
 *  body, but the status is what the UI branches on. Narrow with `instanceof`. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
  }
  /** 401 and 403 have opposite fixes — no credential, versus one lacking a permission — so the
   *  UI has to say which or the reader is left guessing. specs/api-auth.md §8. */
  get isAuth() {
    return this.status === 401 || this.status === 403;
  }
}

/** Who genroc says we are on the last response (`source:subject`), null when it did not say, as
 *  on a 401. Reported, never inferred: behind a proxy we send nothing and still succeed. */
let lastActor: string | null = null;
const actorListeners = new Set<() => void>();
export const actor = () => lastActor;

/** For useSyncExternalStore: the header follows whichever identity the last response named. */
export function onActorChange(listener: () => void): () => void {
  actorListeners.add(listener);
  return () => actorListeners.delete(listener);
}

function setActor(v: string | null) {
  if (v === lastActor) return;
  lastActor = v;
  actorListeners.forEach((l) => l());
}

const session: Middleware = {
  onRequest({ request }) {
    const t = token();
    if (t) request.headers.set("authorization", `Bearer ${t}`);
    return request;
  },
  async onResponse({ response }) {
    // Unconditional, so a 401 CLEARS a stale identity rather than leaving the last good one up.
    setActor(response.headers.get("X-Genroc-Actor"));
    if (!response.ok) {
      const body = (await response.clone().json().catch(() => null)) as components["schemas"]["ApiErrorBody"] | null;
      throw new ApiError(response.status, body?.code ?? "", body?.error ?? `HTTP ${response.status}`);
    }
    return response;
  },
};

const fetchClient = createFetchClient<paths>({ baseUrl: "/api" });
fetchClient.use(session);

/** Every endpoint, typed from openapi.json: `$api.useQuery("get", "/instances", init, options)`.
 *  A query's key is `[method, path, init]`, so `["get", "/tokens"]` invalidates every token read. */
export const $api = createClient(fetchClient);

type Schemas = components["schemas"];
export type Instance = Schemas["ApiInstanceSummaryResp"];
export type ApiToken = Schemas["ApiTokenResp"];
export type Status = Schemas["ModelStatus"];

/** In the order an instance passes through them. A status the spec gains and this list lacks
 *  fails the build on the line below, rather than going missing from the filter. */
export const STATUSES = [
  "running", "pausing", "paused", "failing", "cancelling", "completed", "failed", "raised", "cancelled",
] as const satisfies readonly Status[];
const everyStatusListed: Exclude<Status, (typeof STATUSES)[number]> extends never ? true : never = true;
void everyStatusListed;

/** The five permissions, weakest first (specs/api-auth.md §3). The server rejects anything else
 *  at mint time. */
export const PERMS = ["worker", "read", "operate", "deploy", "admin"] as const;

/** Signs out: clears the session cookie and reloads into the login. Also how someone picks up
 *  changed GROUPS, captured at login; the role map is read per request. Outside the spec: /auth
 *  is genroc-ui's, not genroc's. */
export async function signOut(): Promise<void> {
  await fetch("/auth/logout", { method: "POST" });
  setActor(null);
  setToken("");
  location.href = "/";
}
