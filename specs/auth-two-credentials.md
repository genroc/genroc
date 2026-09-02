# Two credentials, and genroc mints only one of them

Status: **BUILT 2026-09-02.** Revises [api-auth.md](api-auth.md) §2, §5.1 and §6, which has been
reconciled against it — the sections describing `header` mode are kept there as record and marked
where they no longer describe behaviour. Verified end to end against `examples/proxy/`: a browser
holding only a session cookie deploys a definition, and genroc records the actor as
`jwt:alice@genroc.test`.

> **Followed by [ui-component.md](ui-component.md) (proposal, 2026-09-02)**, which moves the
> browser's half into a `genroc-ui` image: the UI leaves the server binary and takes the login
> flow with it, so §3's credential-presence routing and §4's dependence on the proxy's `SameSite`
> both go away. Everything below about what the SERVER accepts is unchanged by it.

## 0. The rule

**genroc accepts exactly two kinds of credential, both on `Authorization: Bearer`:**

| credential | who holds it | genroc's role |
|---|---|---|
| an opaque `genroc_sk_*`, hashed in `api_tokens` | machines — CI, workers, apps, `genctl` | **issues** it, and verifies it |
| a signed JWT, verified against a configured JWKS | people, via whatever IdP the deployment runs | **verifies only** — never issues, never refreshes |

Nothing else is an identity. No trusted headers, no cookies, no client certificates, and **no
path by which a proxy obtains a genroc token on a person's behalf**. A deployment configures
genroc to accept its IdP's tokens correctly; that configuration is the whole integration.

This is api-auth.md §0 unchanged — the deployment owns identity, genroc owns authorization — with
the header-shaped exception removed.

## 1. Why `header` mode goes

It is the only mode whose safety genroc cannot check. Its two failure modes are §6, and the
second is unfixable from inside: a proxy that FORWARDS a client's copy of the identity header
launders a forgery into a trusted assertion, byte-identical on arrival. The defence is a strip
rule in someone else's config, which genroc cannot see, test, or detect the absence of.

**That is §0's own drift argument, turned on authentication.** §0 refuses to push authorization
into ingress rules because the rule lives in a file genroc cannot see and silently rots. Header
mode does exactly that for identity, and unlike the authorization case there is no registry to
keep the two in sync — only a comment in a Caddyfile.

api-auth.md §2.2 defended it as load-bearing, naming three setups that produce no verifiable
token. **A broker answers all three, and the argument does not survive that**, because "produces
no verifiable token" is a property of the *provider*, not of the deployment:

- **GitHub** is OAuth2 with no ID token — but Dex's GitHub connector issues a real OIDC token
  downstream, and populates `groups` as `org:team` when the `groups` scope is requested. That is
  strictly more than header mode ever carried.
- **Google Workspace** omits groups from the ID token, which is what the §2.2 roles overlay was
  built for. A broker fetches them instead, so the overlay is unnecessary.
- **Service meshes** assert a workload identity — machines, which `token` mode already serves,
  and which api-auth.md §2.2 itself says wants a future `mtls` mode reading the *connection*
  rather than a header.

The cost is a component: a deployment whose IdP is not OIDC has to run a broker. That is a real
cost and it is the right one, because it is paid once at setup and is visible, where header
mode's cost is paid silently the first time a proxy is reconfigured.

## 2. Why `/session/token` goes

The exchange exists to solve a *routing* problem, not an identity one, and the routing is what
this design changes.

api-auth.md §5.1 routes `/api/*` **around** the proxy so a machine never meets a login redirect.
That means nothing attaches identity to an API call, so a browser — which holds a proxy cookie
and nothing else — arrives with no credential genroc accepts. `/session/token` is on the proxied
route, so it is the one place the proxy's identity is visible, and it converts that into a bearer
token the SPA can use on the unproxied route.

Under this design the proxy attaches a JWT to every browser request, so the browser never needs
a credential of its own. The exchange has nothing left to do, and it violates §0's rule directly:
it is a proxy obtaining a genroc token.

Deleting it also deletes what it dragged along — `session_ttl`, the mint-on-every-page-load
problem, `session:<subject>` rows accumulating as live credentials, and the SPA's
re-exchange-once-on-401 dance.

## 3. Routing: one host, one API path, two ways in

The split that §5.1 made by PATH is made by **credential presence** instead:

    genroc.example.com/healthz           direct                          unauthenticated probe
    genroc.example.com/public/*          direct                          unauthenticated docs
    genroc.example.com/api/*   has Authorization?  yes -> genroc         machine: token or JWT
                                                   no  -> forward_auth -> oauth2-proxy injects
                                                          Authorization: Bearer <ID token>
    genroc.example.com/*                 forward_auth -> genroc          the UI, behind login

**Bypassing the proxy when `Authorization` is present costs nothing**, because the proxy was
never the security boundary on that branch. genroc validates the credential itself and answers
401 to a bad one; a caller who sets a junk header has bypassed a redirect, not a check.

Two consequences worth stating rather than discovering:

- **The browser holds no credential.** No `localStorage`, no paste field, no PKCE. Same-origin
  `fetch("/api/instances")` carries the cookie, the proxy turns it into a JWT, genroc verifies.
  This is less frontend code than today, not more.
- **`genctl` and workers are unchanged.** They present `genroc_sk_*` and take the direct branch.

## 4. What the proxy must do, and the one thing that must be true

- `--set-authorization-header=true`, so `/oauth2/auth` answers with the ID token, and the router
  copies `Authorization` onto the upstream request.
- `--cookie-refresh` shorter than the ID token's lifetime. Otherwise a long session starts
  401ing mid-use when the token expires and nothing renews it.
- **The session cookie must be `SameSite=Lax` (oauth2-proxy's default) or `Strict`.**

That last one is load-bearing and is the one risk this design introduces. api-auth.md §9 bans
cookies on the control plane because a cookie is *ambient*: a malicious page triggers a
cross-site request, the cookie rides along, and the proxy converts it into a valid credential.
**genroc's own rule survives literally** — it accepts only `Authorization`, and no cookie ever
reaches it — so the CSRF surface moves entirely to the proxy, where `SameSite` closes it by
refusing to attach the cookie to cross-site state-changing requests. It belongs in the example's
router config with this reasoning beside it, because it is invisible when wrong.

## 5. Configuration, after

```yaml
# -auth-config /etc/genroc/auth.yaml     — the only valid mode is `jwt`
mode: jwt
jwt:
  jwks_url: http://dex:5556/dex/keys           # back-channel; jwks_file for air-gapped/testing
  issuer:   http://localhost:5556/dex          # front-channel — what `iss` actually carries
  audience: genroc                             # the IdP client id
  algorithms: [RS256]
  subject_claim: email
  roles_claim:   groups
  leeway: 30s
roles:
  "my-org:platform": [admin]                   # Dex's GitHub connector emits `org:team`
  "*":               [read]
users:
  ada@example.com:  [admin]                    # for providers carrying no groups at all
```

`-auth token` stays an independent flag; a deployment serving both audiences passes both, and the
two authenticators compose in the existing chain.

## 6. What is deleted

Code: `HeaderAuth`, `HeaderModeConfig`, `NewHeaderAuth`, `PrincipalFrom`, `trustedPeer`,
`parseTrustedProxies`; `Server.header`, `SetHeaderAuth`, `Server.assertedHeader`,
`SetAssertedHeader`, `attribute`; `GET /session/token` and `mintSessionToken`;
`JWTAuth.OverlayHeaderRoles` with its `rolesHeader`/`trusted` fields; `AuthConfig.Header` and
`AuthConfig.SessionTTL`.

**`expires_at` on `api_tokens` stays**, against the first draft of this list. Only sessions ever
set it, so it is now written by nothing — but it is a general property of a credential rather
than a session one, dropping it costs a migration, and it is where a `--expires` flag would hang
the day a machine token wants a lifetime. Dead and harmless beats removed and re-added. Frontend: `sessionToken()` and the exchange effect. Example: the `strip_identity`
snippets, which have nothing left to protect.

**§7's asserted attribution goes with them.** It reads an identity header in `none` mode to
record — never to trust — which is defensible on its own terms, but it is the last header-reading
path and its premise (a deployment behind a proxy that has not configured auth) is a state this
design says should not exist. Attribution itself is untouched: `token:`, `jwt:` and `none:` all
still resolve, and `jwt:ada@example.com` is strictly better than the `asserted:` it replaces.

## 6.1 One thing added: `X-Genroc-Actor`

Removing the session exchange broke something that had been riding on it. The UI decided whether
auth was off by inferring — *"the request succeeded and I sent no credential, so there is no
auth"* — which held only while the SPA always carried a token. Behind a proxy it now carries
none and still succeeds, so the inference reads a working login as an unauthenticated server.

Genroc therefore reports the calling principal on every HTTP response, as the same
`source:subject` the audit trail records: `jwt:ada@example.com`, `token:ci`, `none:anonymous`. A
header rather than a `/whoami` endpoint, which was the first design: no extra round trip, nothing
to keep fresh, and no need to decide what permission an endpoint reachable by *every*
authenticated principal should declare. It is absent on a 401 — that absence is what tells a
client to ask for a credential — and present on a 403, where naming the caller is the point.

## 7. What survives unchanged

`token` mode entire, including bootstrap (§5.3) and the token CLI. The role map — `roles:`,
`users:`, `"*"` — and §2.3's split: the IdP says who and what group, genroc says what that may
do. `Perm`/`Allow`/`authorize` and the one gate every transport passes. Attribution (§7). The
path zones and `TestEveryApiPathIsGated` (§1). 401 versus 403 (§8). The token-only deployment
(§5.2), which is the one configuration with no proxy and therefore the one where a person still
pastes a `genroc_sk_*` into the UI.

## 8. Open, and deliberately not decided here

- **mTLS / mesh identity.** Still no OIDC token. A future `mtls` mode reads the connection, not
  a header; until then those callers use `token`.
- **Whether genroc runs the OIDC flow itself** (api-auth.md §10). This design makes the proxy's
  job small and precise — turn a cookie into a JWT — which sharpens that question rather than
  answering it: a deployment still runs one extra component purely to hold a session.
- **PKCE in the SPA.** Would remove the proxy from the browser path entirely and is the other
  way to satisfy §0's rule. Not needed here, because §3 gives the browser a JWT without it.
