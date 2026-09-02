# genroc behind an SSO proxy

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

oauth2-proxy runs the OIDC flow and, on every browser request, turns the session cookie into
`Authorization: Bearer <Dex ID token>`. genroc verifies that signature against Dex's JWKS and
`auth.yaml` turns its claims into permissions. **The browser holds no genroc credential at all**
— nothing in `localStorage`, nothing to mint, nothing to expire.

The API is reached two ways and the router picks on one thing: whether the request already
carries a credential. A machine's `genroc_sk_*` goes straight through, so a script never meets a
login redirect; everything else goes via the cookie. Bypassing the proxy on that branch costs
nothing — genroc validates the credential itself and 401s a bad one.

Verified against the running stack: a browser holding only a cookie deploys a definition, and
genroc records the actor as `jwt:alice@genroc.test`.

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
**200 with full admin**. The defence then was a strip rule in the Caddyfile, which genroc could
neither see nor test — a forwarded header and a laundered one are byte-identical on arrival. That
is why the mode is gone: its safety lived in a file genroc had no way to check.
specs/auth-two-credentials.md §1.

### What moved instead: the cookie

The browser's session cookie is now the ambient credential, and ambient credentials are what
CSRF exploits. genroc never sees it — it only ever reads `Authorization` — so the risk sits
entirely with oauth2-proxy, and `--cookie-samesite=lax` is what closes it: the browser refuses to
attach the cookie to a cross-site state-changing request, so a hostile page cannot make you
deploy. It is set explicitly in `compose.yaml` because it is invisible when wrong.

## The pieces

    Caddy         routes on whether a request already carries a credential
    oauth2-proxy  runs the OIDC flow; turns the cookie into a Bearer ID token
    Dex           the identity provider, users in dex/config.yaml
    genroc        verifies JWTs for people, issues tokens for machines

Caddy authenticates nothing itself — it asks oauth2-proxy, which answers 202 carrying the ID
token, or 401. That keeps routing in one file and OIDC in the component that specialises in it.

### Five things that cost an hour each

**`docker compose restart` used to leave you with no genroc, and it looked like an auth bug.**
`depends_on: condition: service_healthy` is honoured on `up` and NOT on `restart`, which restarts
everything at once — so genroc raced Postgres, lost, and exited 1 the way it is designed to when
it cannot open its database. Nothing brought it back, and Caddy then answered **502 to every
request** because the hostname stopped resolving. `restart: unless-stopped` on the genroc service
is the supervisor its own `main.go` assumes exists. If you see a blanket 502, check
`docker compose ps` for a service that is simply not there before suspecting the login.

**Dex's storage has to persist, and it is not about durability.** Dex keeps its SIGNING KEYS in
storage, so `type: memory` regenerates them on every restart — which breaks authentication in two
directions at once, both presenting as "it randomly stopped working":

- *Your browser holds a token signed by a key that no longer exists.* genroc has since fetched
  the new JWKS and cannot verify it, ever. oauth2-proxy keeps vouching for the session, so its
  log shows a cheerful `202` with your email while every API call 401s. Only a fresh login clears
  it — which is why deleting the cookie "fixes" it.
- *genroc holds the old JWKS and the token is new.* It refreshes on an unknown `kid`, but
  rate-limited to once per five minutes (which is what stops junk `kid`s becoming a fetch storm),
  so there is a window where nothing works and nothing is wrong.

`dex/config.yaml` therefore uses `sqlite3` on a named volume, and the keys survive restarts. If
you ever swap it back to `memory`, expect both symptoms.

**The `groups` scope fails silently.** Dex emits membership only when the scope asks. Without it
every caller arrives role-less and falls to `"*"` — a 403 that looks like a broken role map.

**oauth2-proxy sends `approval_prompt=force`**, overriding Dex's `skipApprovalScreen`: Dex logs
*"config skipping approval screen"* and renders it anyway. Use `--approval-prompt=auto`.

**`--cookie-secret` must decode to 16/24/32 bytes.** `openssl rand -base64 32` fails twice — 44
characters, and the wrong alphabet. Use `openssl rand -hex 16`.

## What this trades away

`staticPasswords` buys one container and a user list in the file you are already reading. It
costs group membership, an admin UI, self-service, password reset and MFA. When that stops
fitting: a `connectors:` entry federates a directory you have; LLDAP + Dex adds a small one
(~1.5 MB, its whole UI is users and groups); Keycloak covers MFA and custom flows at ~507 MB.

**genroc does not change between any of them** — all it ever sees is a signed token and a JWKS
to check it against.
