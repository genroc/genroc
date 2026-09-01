// The genroc client. Every request is same-origin — the Vite dev server proxies /api, and in
// production genroc serves this app itself — so there is no base URL here and no CORS to
// configure anywhere.
//
// The credential is a bearer token kept in localStorage. Behind a proxy it comes from the
// session exchange (see sessionToken below); without one the user pastes it. Both end up in the
// same place, so nothing else in this file cares which happened.

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

async function call<T>(path: string, init?: RequestInit): Promise<T> {
  const t = token();
  const headers: Record<string, string> = init?.body ? { "content-type": "application/json" } : {};
  if (t) headers.authorization = `Bearer ${t}`;
  const res = await fetch(path, { ...init, headers });
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

/** Asks the proxy-backed session exchange for a token. Returns "" when there is no proxy, no
 *  identity, or the server has no header mode — all of which mean "fall back to a pasted
 *  token" rather than "fail". specs/api-auth.md §9.
 *
 *  This is a same-origin GET whose response must never be readable cross-origin: the browser
 *  authenticates it with the proxy's ambient cookie, so what stops a hostile page stealing the
 *  token is that it cannot read the reply. Do not add CORS headers to /session/token. */
export async function sessionToken(): Promise<{ token: string; subject: string } | null> {
  try {
    const res = await fetch("/session/token", { cache: "no-store" });
    if (!res.ok) return null;
    return (await res.json()) as { token: string; subject: string };
  } catch {
    return null;
  }
}

export const listDefinitions = () =>
  get<Page<{ name: string; version: number; created_at: string }>>("/api/definitions");
