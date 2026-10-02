# Two credentials, and genroc mints only one of them

Status: **Built.**

## 0. The rule

**genroc accepts exactly two kinds of credential, both on `Authorization: Bearer`:**

| credential | who holds it | genroc's role |
|---|---|---|
| an opaque `genroc_sk_*`, hashed in `api_tokens` | machines — CI, workers, apps, `genctl` | **issues** and verifies it ([api-auth.md](api-auth.md) §5) |
| an HS256 JWT minted by genroc-ui | people | **verifies only** ([ui-issued-tokens.md](ui-issued-tokens.md)) |

Nothing else is an identity: no trusted headers, no cookies, no client certificates, and **no
path by which a proxy obtains a genroc token on a person's behalf**. This is api-auth.md §0 — the
deployment owns identity, genroc owns authorization — with no header-shaped exception.

## 1. Why there is no trusted-header mode

A mode that believes an identity header set by a proxy is sound only while genroc is unreachable
except through that proxy, and only while the proxy strips a client's own copy of the header — a
forwarded header and a laundered one are byte-identical on arrival. Both defences live in config
genroc cannot see, test, or detect the absence of: api-auth.md §0's drift argument, turned on
authentication. A signed token makes a bypass worthless instead (api-auth.md §2.1).

"My provider issues no verifiable token" is a property of the provider, not the deployment, and a
broker answers it: Dex's GitHub connector issues a real OIDC token carrying `org:team` groups,
and genroc-ui fetches Google Workspace groups itself (`type: google`). Mesh and mTLS identities
are machines, which `token` serves. The cost is running a broker for a non-OIDC IdP — paid once,
and visibly.

## 2. Why there is no session-exchange endpoint

An endpoint turning a browser's session into a `genroc_sk_*` would be a proxy obtaining a genroc
token, which §0 forbids, and would leave a live credential row behind on every page load. genroc-ui
mints a short-lived JWT per request instead, so the SPA holds no credential
([ui-issued-tokens.md](ui-issued-tokens.md) §4).

## 3. Routing

Browsers reach genroc through genroc-ui and machines reach it directly, so nothing routes on what a
request carries ([ui-component.md](ui-component.md) §3).

## 4. CSRF

Folded into [ui-component.md](ui-component.md) §2: no cookie reaches genroc, and the session
cookie's `SameSite=Lax` is set by genroc-ui's code.

## 6. `expires_at` stays, written by nothing

Every token is a machine credential, and rotating one is a deploy, not a clock, so nothing sets
`api_tokens.expires_at`. The column stays because a lifetime is a property of a credential, not of
a session, and dropping it costs a migration (§8). `GetAPITokenByHash` and `CountLiveAdminTokens`
exclude expired rows in SQL, so an expired admin token neither authenticates nor satisfies
bootstrap (api-auth.md §5.3).

## 6.1 `X-Genroc-Actor`

Every HTTP response carries the calling principal as `X-Genroc-Actor: source:subject`, the same
string the audit trail records, because a client cannot infer it: behind genroc-ui the SPA sends
no credential of its own and still succeeds, exactly as against a server with no auth. A header,
not a `/whoami` endpoint: no extra round trip, nothing to keep fresh, and no permission to choose
for an endpoint every principal may reach. **Absent on a 401** — the absence tells a client to ask
for a credential — and **present on a 403**, where naming the caller is the point. HTTP only.

## 8. Open

- **`--expires` on `token create`** — the day a machine token wants a lifetime; it writes
  `expires_at`, which §6 keeps for it.
- **An `mtls` mode** — when mesh-identified machines cannot hold a `token`. It reads the
  connection, never a header.
