// The genroc client. Every request is same-origin — the Vite dev server proxies /api, and in
// production genroc serves this app itself — so there is no base URL here and no CORS to
// configure anywhere.
//
// The credential, when this app holds one, is a bearer token in localStorage — a genroc token
// or a JWT, since genroc accepts either (specs/auth-two-credentials.md).
//
// Behind an SSO proxy it holds NONE: the proxy turns the browser's session cookie into a JWT
// and attaches it to every request, so sending nothing is correct there. The stored value is
// the no-proxy case, where a person pastes their own credential.

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

/** Who genroc says we are on the last response, as `source:subject` — or null when it did not
 *  say, which is what a 401 looks like. The server reports it on every reply because a client
 *  cannot infer it: behind a proxy this app sends no credential and still succeeds, which is
 *  indistinguishable from `-auth none` unless someone says so. */
let lastActor: string | null = null;
export const actor = () => lastActor;

async function call<T>(path: string, init?: RequestInit): Promise<T> {
  const t = token();
  const headers: Record<string, string> = init?.body ? { "content-type": "application/json" } : {};
  if (t) headers.authorization = `Bearer ${t}`;
  const res = await fetch(path, { ...init, headers });
  // Unconditional, so a 401 CLEARS a stale identity rather than leaving the last good one up.
  lastActor = res.headers.get("X-Genroc-Actor");
  const text = await res.text();
  const body = text ? JSON.parse(text) : null;
  if (!res.ok) throw new ApiError(res.status, body?.code ?? "", body?.error ?? `HTTP ${res.status}`);
  return body as T;
}

const get = <T,>(path: string) => call<T>(path);

export type Instance = {
  id: string;
  process: string;
  version: number;
  status: string;
  task?: string;
  wait_state?: string;
  error_code?: string;
  error_message?: string;
  created_at: string;
  updated_at: string;
};

export type Page<T> = { items: T[] | null; page: { after?: string } };

export const listInstances = (q: string) => get<Page<Instance>>(`/api/instances${q ? `?${q}` : ""}`);

export const getInstance = (id: string) =>
  get<Instance & { state?: Record<string, unknown> }>(`/api/instances/${encodeURIComponent(id)}/detail`);

/** The five permissions, in the order specs/api-auth.md §3 introduces them — weakest inbound
 *  zone first, admin last. The server rejects anything outside this set at mint time rather
 *  than letting a typo become a 403 somewhere unrelated. */
export const PERMS = ["worker", "read", "operate", "deploy", "admin"] as const;

export type ApiToken = {
  id: string;
  label?: string;
  perms: string[];
  created_at: string;
  last_used_at?: string;
  revoked_at?: string;
  /** Absent for a machine token, which never expires. A session token always has one. */
  expires_at?: string;
};

export const listTokens = () => get<{ items: ApiToken[] | null }>("/api/tokens");

/** The response carries the secret, and it is the only time it exists anywhere — the row stores
 *  a hash and cannot produce it again. A caller that drops this value has to mint another. */
export const createToken = (label: string, perms: string[]) =>
  call<{ id: string; token: string; label?: string; perms: string[] }>("/api/tokens", {
    method: "POST",
    body: JSON.stringify({ label, perms }),
  });

export const revokeToken = (id: string) =>
  call<{ revoked: boolean }>(`/api/tokens/${encodeURIComponent(id)}`, { method: "DELETE" });

/** Signs out: clears the session cookie and reloads into the login.
 *
 *  A POST, because it changes state — a GET would be reachable from any page that can make the
 *  browser follow a link. It is also how someone picks up a change to their own GROUPS, which
 *  are captured at login and carried in the cookie; the role map is read per request and needs
 *  no sign-out. */
export async function signOut(): Promise<void> {
  await fetch("/auth/logout", { method: "POST" });
  lastActor = null;
  setToken("");
  location.href = "/";
}

export const listDefinitions = () =>
  get<Page<{ name: string; version: number; created_at: string }>>("/api/definitions");
