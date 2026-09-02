# genroc with a login

One hostname, two audiences: people who log in, and machines that present tokens.
specs/api-auth.md §5.1.

    ./examples/proxy/gen-env.sh                      # once — the evaluator's worker token
    docker compose -f examples/proxy/compose.yaml up --build

| user | password | `auth.yaml` | may |
|---|---|---|---|
| `alice@genroc.test` | `demo` | `users: [admin]` | everything |
| `carol@genroc.test` | `s3cret` | `users: [operate,read]` | start/pause/retry, not deploy |
| `bob@genroc.test` | `hunter2` | `"*": [read]` | read only |

Measured on this stack:

    user     GET /instances   POST /instances   PUT /definitions
    alice    200              200               200
    carol    200              200               403
    bob      200              403               403

**The genroc-relevant part is `auth.yaml`, and it is 30 lines.** Everything else is one way of
producing a signed token; yours will differ.

## What happens

    GET /                                302 → Dex login → 200
    GET /api/instances  (cookie only)    proxy attaches Bearer <ID token> → 200
    GET /api/instances  (Bearer …)       straight to genroc, no proxy in the path

**genroc-ui** serves the UI, runs the OIDC login against Dex, keeps the resulting ID token in a
session cookie, and attaches it to every API call it proxies. genroc verifies that signature
against Dex's JWKS and `auth.yaml` turns its claims into permissions. **The browser holds no
genroc credential at all** — nothing in `localStorage`, nothing to mint, nothing to expire.

Browsers and machines never share a route: a person talks to genroc-ui on `:8448`, the evaluator
talks to genroc directly inside the network, and genroc's own port is not published. Nothing has
to route on what a request carries, because a component that only serves browsers can never
answer a script with a login page.

Verified against the running stack: a browser holding only a cookie deploys a definition, genroc
records the actor as `jwt:alice@genroc.test`, and `/auth/logout` puts the next call back to 401.

## Adding people

Two edits: a `staticPasswords` entry in `dex/config.yaml` (bcrypt: `caddy hash-password`), then
a line under `users:` in `auth.yaml`. Both need a restart.

**The second edit is the price of `staticPasswords`**, which carries no group membership — so
the ID token has no `groups` claim and `roles:` can grant nobody anything beyond `*`. Swap it for
a `connectors:` entry (commented in `dex/config.yaml`) and that edit disappears: `auth.yaml`
names groups, membership lives in the directory.

**Dex is also the answer for a provider that is not OIDC at all.** GitHub speaks OAuth2 and
issues no ID token, so genroc cannot verify it directly — but Dex's GitHub connector federates it
and mints a proper OIDC token downstream, carrying the user's org and team membership as
`groups` in `org:team` form. So the role map reads `"my-org:platform": [admin]`, and genroc's
side is unchanged. That is the whole reason genroc needs no non-OIDC code path.

## Connecting genctl

genctl uses a genroc token; it cannot use your browser session, which lives in a proxy cookie.
**Mint one in the UI** — log in, tokens tab, tick permissions, name it:

    export GENROC_TOKEN=genroc_sk_...

No `GENROC_SERVER`: the proxy listens on **8448**, genroc's own default, so genctl finds it
without being told and the proxy is invisible in every URL.

Nothing is seeded for people: the browser path produces an admin session and minting is an admin
action. Break-glass, if the UI is unreachable:

    docker compose -f examples/proxy/compose.yaml exec genroc \
      genroc token create --perms admin -pg 'postgres://genroc:genroc@postgres:5432/genroc?sslmode=disable'

## The evaluator, and the credential a person cannot mint

    genctl apply -f examples/proxy/script-node.genroc.yaml -f examples/proxy/hello-script-node.genroc.yaml
    genctl run hello-script --set name=genroc      → { "greeting": "hello, genroc" }

A worker starts with the stack, no human present, before anything exists to mint from — that is
why `gen-env.sh` and `-seed-tokens` are here. It is not that machines are special; boot order is.

Its token is `worker` and nothing else:

    POST /external-tasks/claim   200
    GET  /instances              403
    PUT  /definitions            403
    GET  /tokens                 403

Two traps writing a script: it is a **function body**, not a module (`return`, not
`export default`), and `${` must be escaped as `$${` or genroc's value layer resolves the
template literal as its own interpolation — caught at apply, naming the task and field.

## The part that is actually security

**The guarantee is in the request, not in the network position.** genroc verifies a signature it
can check — issuer, audience and algorithm all pinned, refused at startup if unset — so reaching
:8448 directly buys nothing. There is no identity header to forge:

    curl -X PUT -H 'X-Auth-Request-Email: mallory@evil.test' … /api/definitions  → 401

An earlier version of this example ran genroc in `header` mode, and that same request returned
**200 with full admin**. The defence then was a strip rule in the router, which genroc could
neither see nor test — a forwarded header and a laundered one are byte-identical on arrival. That
is why the mode is gone: its safety lived in a file genroc had no way to check.
specs/auth-two-credentials.md §1.

### What moved instead: the cookie

The browser's session cookie is now the ambient credential, and ambient credentials are what
CSRF exploits. genroc never sees it — it only ever reads `Authorization` — so the risk sits
entirely with genroc-ui — which is the point of shipping that component rather than wiring up a
general-purpose one. It sets `SameSite=Lax` and `HttpOnly` itself, so the browser refuses to
attach the cookie to a cross-site state-changing request and no script can read it. Correct by
construction rather than by a line in someone else's config.

## The pieces

    genroc-ui   serves the UI, runs the OIDC login, proxies /api/* with the ID token attached
    Dex         the identity provider, users in dex/config.yaml
    genroc      verifies JWTs for people, issues tokens for machines
    postgres    genroc's database
    evaluator   the script-task worker; talks to genroc directly with its own token

**This used to be six services.** Caddy routed on whether a request already carried a credential
and oauth2-proxy ran the flow, wired together with `forward_auth`. genroc-ui replaced the pair,
and the credential-presence routing went with them — it existed only because one port served both
audiences. specs/ui-component.md §3.

### Five things that cost an hour each

**`docker compose restart` used to leave you with no genroc, and it looked like an auth bug.**
`depends_on: condition: service_healthy` is honoured on `up` and NOT on `restart`, which restarts
everything at once — so genroc raced Postgres, lost, and exited 1 the way it is designed to when
it cannot open its database. Nothing brought it back, and the router then answered **502 to every
request** because the hostname stopped resolving. `restart: unless-stopped` on the genroc service
is the supervisor its own `main.go` assumes exists. If you see a blanket 502, check
`docker compose ps` for a service that is simply not there before suspecting the login.

**Dex's storage has to persist, and it is not about durability.** Dex keeps its SIGNING KEYS in
storage, so `type: memory` regenerates them on every restart — which breaks authentication in two
directions at once, both presenting as "it randomly stopped working":

- *Your browser holds a token signed by a key that no longer exists.* genroc has since fetched
  the new JWKS and cannot verify it, ever. Only a fresh login clears it — which is why deleting
  the cookie "fixes" it.
- *genroc holds the old JWKS and the token is new.* It refreshes on an unknown `kid`, but
  rate-limited to once per five minutes (which is what stops junk `kid`s becoming a fetch storm),
  so there is a window where nothing works and nothing is wrong.

`dex/config.yaml` therefore uses `sqlite3` on a named volume, and the keys survive restarts. If
you ever swap it back to `memory`, expect both symptoms.

**The `groups` scope fails silently.** Dex emits membership only when the scope asks. Without it
every caller arrives role-less and falls to `"*"` — a 403 that looks like a broken role map.

**An OIDC issuer is one URL resolved from two places.** The browser must reach `localhost:5556`
for the login form; genroc-ui must reach `dex:5556` to exchange the code and fetch keys.
Discovery publishes one set and cannot satisfy both, which is why `OIDC_DISCOVERY_URL`,
`OIDC_TOKEN_URL` and `OIDC_JWKS_URL` name the back-channel endpoints separately. `OIDC_ISSUER`
stays the browser-facing value, because that is what Dex stamps into `iss`.

## What this trades away

`staticPasswords` buys one container and a user list in the file you are already reading. It
costs group membership, an admin UI, self-service, password reset and MFA. When that stops
fitting: a `connectors:` entry federates a directory you have; LLDAP + Dex adds a small one
(~1.5 MB, its whole UI is users and groups); Keycloak covers MFA and custom flows at ~507 MB.

**genroc does not change between any of them** — all it ever sees is a signed token and a JWKS
to check it against. Nor does genroc-ui: swapping the provider is `OIDC_ISSUER` and a client id.
