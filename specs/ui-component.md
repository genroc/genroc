# The UI is a separate component, and it owns the login

Status: **Built.** `ui/` is its own Go module, `ghcr.io/genroc/ui` its own image, and `examples/ui/`
runs the pair.

## 0. The split

**The genroc server is an API and nothing else**: no UI, no login flow, no cookie. It verifies a
`genroc_sk_*` it issued or a JWT genroc-ui minted ([auth-two-credentials.md](auth-two-credentials.md) §0).

**`genroc-ui`** serves the SPA, runs the login, holds the session, and proxies `/api/*`,
`/healthz` and `/public/*` to the server with a bearer token attached.

    browser  ->  genroc-ui   serves the SPA, runs the login, holds the session cookie,
                             attaches a minted `Authorization: Bearer {sub, perms}`, proxies /api/*
                     |
                     v
                  genroc     verifies the JWT, reads `perms` (ui-issued-tokens.md)

    genctl, workers, your own service  ->  genroc directly, with `genroc_sk_*`

## 1. Why the server sheds the UI

**The server is meant to be embedded** in somebody's service, which has no use for a monitoring UI
in its address space, while the UI is free to grow. Two boundaries keep that growth out:

- **The image.** Only `ui/Dockerfile` has a Node stage; the server's image needs no JavaScript
  toolchain.
- **The module.** `ui/go.mod` requires nothing from the server's module, so the UI cannot quietly
  acquire the server's ~20 dependencies, and the server binary links none of the UI's. The repo
  is a `go.work` workspace as a result, and `./...` matches one module only — the Makefile and CI
  name `./... ./ui/...`.

## 2. Why the login lives here and not in the server

The flow has to live somewhere, and in the server every embedded deployment would carry redirect
URIs, cookie lifetime and CSRF handling it has no use for (api-auth.md §9). In genroc-ui **the
server still never sees a cookie**, and the session cookie — the one ambient credential, and so
the whole CSRF surface — is `HttpOnly` and `SameSite=Lax` by code we ship and test, not by a line
in a third-party proxy's config, which is the objection that rules out trusted headers
(auth-two-credentials.md §1).

## 3. What genroc-ui does with a request

`/healthz` and `/public/*` are proxied ungated: the server serves them without a credential, and
gating them made `/healthz` 401 through the UI while the server answered 200. For `/api/*`, the
first case that matches wins:

1. **`Authorization` already present** — passed through untouched. On a deployment with no login
   configured a person pastes a `genroc_sk_*` into the SPA, which keeps it in `localStorage`;
   genroc-ui never judges a credential, the server does.
2. **A valid session cookie** — resolve the session's groups through the role map, mint a
   short-lived token, attach it as `Authorization: Bearer` ([ui-issued-tokens.md](ui-issued-tokens.md) §4).
3. **Neither** — with a login configured, a document request (`Accept: text/html`) is redirected
   to `/auth/login` and an XHR gets **401**, since a fetch cannot follow a login redirect usefully.
   With no login configured, it passes through unauthenticated: the laptop case, against a server
   with no auth or a pasted token.

The app's own document is behind the session too; the built bundle files, including the login
page, are public. Because browsers and machines arrive at different components, nothing has to
route on what a request carries.

## 4. Session shape

- The session — a genroc-ui-signed JWT `{sub, groups}` in a cookie — is ui-issued-tokens.md §4.
  No server-side store, no database, no user directory: the cookie holds the credential and
  expires with it. `Secure` follows the request's scheme (TLS or `X-Forwarded-Proto`), or
  `secure_cookie` forces it.
- `state`, `nonce`, the provider and the return path ride in 10-minute cookies across the
  redirect; the callback refuses a state mismatch, and the return path refuses absolute URLs and
  `/auth/` targets.
- `/auth/login` skips the chooser when there is exactly one provider and no passwords.
- **On expiry, the browser goes back through the login**, silently while the provider's session is
  alive, which is what makes no refresh tokens acceptable. Adding refresh later means storing one, and
  that is the first thing here that would need persistence -- so it is deliberately not in v1.
- Logout (`POST /auth/logout`) clears the cookie. RP-initiated logout at the IdP is not built.
- **A confidential client**, not PKCE with a public client: genroc-ui is a server, so it can hold
  a client secret, which is both stronger and simpler.
- Password login (`POST /auth/password`) is throttled per email (10 failures per 5 minutes) and per
  address (60), checked before hashing, with one message whichever limit tripped.

## 5. What genroc-ui does NOT do

It holds the group → permission map (ui-issued-tokens.md §5) but not the endpoint → permission
mapping, which stays on `actionDef.Allow`. The SPA learns its identity from `X-Genroc-Actor`
(auth-two-credentials.md §6.1). It is **not on the machine path**: workers, `genctl` and an
embedding service talk to the server directly, so nothing scripted ever meets a login page.

## 5.1 A relying party, not a broker

genroc-ui logs in against any OIDC provider with discovery and a client secret (`ui/oidc`), plus
`type: google`, whose Workspace groups it fetches at login with the person's own access token. A
provider with no ID token — GitHub, LDAP, SAML — needs a broker such as Dex in front. genroc-ui
signs only its own token for genroc (ui-issued-tokens.md); an issuer with connectors *is* Dex,
and rebuilding it would cost auth-two-credentials.md §1 its answer that a broker covers every
non-OIDC provider. The signal to reopen this is not "someone asked for
GitHub"; it is a deployment that cannot run a broker at all.

**Not embedded Dex**, though `dexidp/dex/server.NewServer` returns an `http.Handler`: it persists
signing keys, giving the component a database; it lands SAML, LDAP, etcd and client-go in `ui/`'s
module graph; a Dex advisory becomes our release instead of an image-tag bump; and it optimises the
minority case, since nearly every IdP is OIDC and the default deployment is genroc + genroc-ui
with no Dex.

**The complaint that motivates it is packaging, and packaging answers it**: a compose file or
chart that bundles Dex for the people who need it, while the two-container path stays default.
The reopen signal is a **single-binary deployment** -- genroc on a VM under systemd, no
orchestrator -- where a second PROCESS is the obstacle rather than a second line of YAML.

genroc-ui holds a session for a login someone else performed, or checks a bcrypt hash from its
config (ui-issued-tokens.md §5); it does not own accounts.

## 7. Build

Node builds the SPA (two bundles, the app and the login page); `go:embed all:web` compiles it into
a single distroless binary, so there is no path to misconfigure. `ui/web/` holds a committed
placeholder so `go build` works without Node, and only the image build swaps the built SPA in.

## 8. Open

- **The dev loop.** `ui/frontend/`'s Vite dev server proxies to genroc today and can keep doing so
  with auth off. Whether `npm run dev` should instead point at a local genroc-ui, so the login
  path is exercised in development, is unsettled.
