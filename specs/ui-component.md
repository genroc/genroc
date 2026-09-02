# The UI is a separate component, and it owns the login

Status: **PROPOSAL 2026-09-02, nothing built.** Follows
[auth-two-credentials.md](auth-two-credentials.md), which left the browser's credential coming
from a proxy sandwich. This replaces that sandwich with one component genroc ships, and moves the
UI out of the server binary.

> **§5.1 is reversed by [ui-issued-tokens.md](ui-issued-tokens.md) (proposal, 2026-09-02).**
> That doc has genroc-ui MINT its own token rather than relay the provider's, which moves the
> role map out of the server and makes the token format a published contract. The reasoning below
> for keeping it a relay is kept because the distinction it draws still holds — what changed is
> that a *local* issuer is not the same thing as rebuilding a broker's connectors.

## 0. The split

**The genroc server is an API and nothing else.** No UI, no login flow, no cookie. It verifies a
`genroc_sk_*` it issued or a JWT it was configured to trust, and that is the whole of its
identity surface (auth-two-credentials.md §0).

**`genroc-ui` is a separate image** that serves the SPA, runs the OIDC login, holds the session,
and proxies `/api/*` to the server with a bearer token attached.

    browser  ->  genroc-ui   serves the SPA, runs OIDC, holds the cookie,
                             attaches `Authorization: Bearer <ID token>`, proxies /api/*
                     |
                     v
                  genroc     verifies the JWT, applies the role map -- unchanged

    genctl, workers, your own service  ->  genroc directly, with `genroc_sk_*`

## 1. Why the server sheds the UI

**The server binary should stay small, because it is meant to be embedded.** genroc is run as
part of somebody's service, and a monitoring UI is not something that deployment needs in its
address space. The UI, by contrast, is free to grow -- dashboards, filters, graphs -- and none of
that growth should reach the thing running the engine.

The cost today is not the ten lines of `-ui` handler. It is that the server's image carries an
entire **Node build stage** (`npm ci`, `npm run build`) purely to bake `/srv/ui` in, so building
the server requires a JavaScript toolchain and a frontend it does not use. Splitting the image
removes that stage from the server outright.

## 2. Why the login lives here and not in the server

api-auth.md §10 asked whether genroc should run the OIDC flow itself. The answer is **no, but
something we ship should**, and the distinction is the point: the flow has to exist somewhere,
and putting it in the server would make every embedded deployment carry redirect URIs, cookie
lifetime and CSRF handling it has no use for -- exactly the surface §9 says the server is not in
the business of.

Putting it in `genroc-ui` keeps §9 literally true: **the server still never sees a cookie.** And
it closes the one gap auth-two-credentials.md left open. That design's single risk (§4) is that
`SameSite` protects the browser path while living in oauth2-proxy's config, where genroc cannot
see it, test it, or notice its absence -- the same criticism that retired `header` mode. A cookie
set by a component we ship is correct by construction.

## 3. What genroc-ui does with a request

Three cases, in order, and the first that matches wins:

1. **`Authorization` already present** -- pass it through untouched. This is what makes a pasted
   `genroc_sk_*` work against a UI with no IdP configured, and it means genroc-ui never has to
   decide whether a credential is good; the server does.
2. **A valid session cookie** -- attach the ID token from it as `Authorization: Bearer`.
3. **Neither** -- if OIDC is configured, redirect to the IdP for a document request and answer
   **401** for an XHR (a fetch cannot follow a login redirect usefully; the SPA reloads instead).
   If OIDC is NOT configured, pass through unauthenticated, which is the laptop case against a
   server running `-auth none`.

Case 1 is why the credential-presence matcher disappears rather than moving: browsers and
machines now arrive at *different components*, so nothing has to route on what a request carries.

## 4. Session shape

- The IdP's **ID token in an `HttpOnly`, `SameSite=Lax`, `Secure` cookie**. No server-side session
  store, no database, no user directory -- the cookie holds the credential and expires with it.
- `state` and `nonce` ride in short-lived cookies across the redirect, and the callback refuses a
  mismatch.
- **On expiry, redirect to the IdP again.** Silent while the user's SSO session is alive, which is
  what makes this acceptable without refresh tokens. Adding refresh later means storing one, and
  that is the first thing here that would need persistence -- so it is deliberately not in v1.
- Logout clears the cookie. RP-initiated logout at the IdP is optional and configured, not assumed.
- **A confidential client**, not PKCE-with-a-public-client: genroc-ui is a server, so it can hold
  a client secret, which is both stronger and simpler.

## 5. What genroc-ui does NOT do

It does not verify permissions, hold a role map, or interpret genroc's model. It attaches a
credential and forwards. Everything about who may do what stays in the server, where
`auth-two-credentials.md` put it -- and the SPA learns its own identity from `X-Genroc-Actor` on
any response, so genroc-ui does not have to tell it either.

It is also **not on the machine path**. Workers, `genctl` and an embedding service talk to the
server directly. A component that only ever serves browsers cannot answer a script with a login
page, which is the failure api-auth.md §5.1 spent its routing rules avoiding.

## 5.1 It relays, it never issues -- which is what bounds it

The obvious next ask is "log in with Google, log in with GitHub, without running Dex". Half of
that is free and half of it is Dex.

**Google is free**, and so is almost everything else: Google is a real OIDC provider, as are
Okta, Entra, Auth0, Keycloak, Zitadel, Authentik and Dex. An ordinary relying party talks to any
of them with discovery and a client secret. Supporting "most IdPs" costs nothing beyond §4.

**GitHub is not**, and the reason it is expensive is not the connector. GitHub speaks OAuth2 and
issues no ID token, so there is nothing to relay: genroc-ui would run the OAuth2 flow, call
`/user` and `/user/teams` itself, and then have an identity with no token attached to it. To
reach the server it would have to **mint** one -- which means holding a signing key, publishing a
JWKS, and having genroc's `jwks_url` point at genroc-ui.

That is the line. A relay forwards a token someone else signed and holds no key; an issuer signs.
**An issuer with connectors is Dex**, and rebuilding it would cost us the argument
[auth-two-credentials.md](auth-two-credentials.md) §1 rests on -- that a broker answers every
non-OIDC provider, which is only a good answer while we are not the broker.

So: **genroc-ui holds no signing key.** A provider that is not OIDC needs a broker in front, and
the list is short -- GitHub, LDAP, SAML. The signal to reopen this is not "someone asked for
GitHub"; it is a deployment that cannot run a broker at all.

**Embedding Dex as a library was considered and rejected.** It is possible --
`dexidp/dex/server.NewServer` returns an `http.Handler` -- and it would answer the connector half
without us writing one. Four reasons not to, in order of weight:

- **Storage comes back.** Embedded Dex still persists signing keys, which is exactly the failure
  that produced §4's cookie behaviour and the example's `dexdata` volume. genroc-ui would gain a
  database requirement, added to the component whose whole pitch is being one simple thing.
- **It optimises the exception.** With genroc-ui as a relying party the DEFAULT deployment is
  genroc + genroc-ui and no Dex, because nearly every IdP is OIDC. Embedding spends the cost on
  the minority case.
- **The dependency graph lands on the whole module** -- SAML, LDAP, etcd, client-go, several SQL
  drivers -- even though the server binary never links it. A separate Go module would contain
  that, but that is a decision in itself.
- **CVEs move from an image tag into a release.** As a container, a Dex advisory is a version
  bump; embedded, it is ours to ship.

It also does not avoid the line above: embedded or not, genroc-ui would mint and publish a JWKS.
Embedding only saves writing connectors, and the signing key was the expensive part.

**The complaint that motivates it is packaging, and packaging answers it**: a compose file or
chart that bundles Dex for the people who need it, while the two-container path stays default.
The reopen signal is a **single-binary deployment** -- genroc on a VM under systemd, no
orchestrator -- where a second PROCESS is the obstacle rather than a second line of YAML.

**User management is a harder no** and is api-auth.md §9's first line: no users, no passwords, no
sessions, no password reset. genroc-ui holds a session cookie for a login someone else performed,
which is a different thing from owning an account.

## 6. What this deletes

From the server: the `-ui` flag, `Server.SetUI`, `Server.uiDir` and the root file handler; the
`node:24-alpine AS ui` stage, the `COPY --from=ui /ui/dist /srv/ui` and the `CMD ["-ui", …]` in
`Dockerfile`.

From `examples/proxy/`: **Caddy and oauth2-proxy both**, with `Caddyfile`, the forward_auth
wiring, the `@credentialed` matcher, `--set-authorization-header`, `--cookie-samesite`,
`--cookie-refresh` and the cookie-secret footgun. The stack becomes **genroc + genroc-ui + Dex**
(+ the evaluator, which is the machine half of the story). Two of the five "things that cost an
hour each" in its README are about oauth2-proxy flags and go with it.

## 7. Build

Node compiles the SPA, Go builds a small server that embeds it, distroless like genroc -- one
toolchain pattern, two images. `go:embed` rather than a copied directory, so the UI image is a
single binary and there is no path to misconfigure.

## 8. Open

- **The dev loop.** `frontend/`'s Vite dev server proxies to genroc today and can keep doing so
  with auth off. Whether `npm run dev` should instead point at a local genroc-ui, so the login
  path is exercised in development, is unsettled.
- **Does genroc-ui proxy or redirect for `/public/*` and `/healthz`?** They are unauthenticated on
  the server, so either works; proxying keeps one origin, which is the reason CORS does not exist
  anywhere in this design.
- **Config surface.** Issuer, client id/secret, and the genroc address are the minimum. Whether it
  reads the same `auth.yaml` shape as the server or its own is not decided.
