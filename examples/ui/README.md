# genroc with a login

Two audiences on one stack: people who sign in, and machines that present tokens. Neither can
use the other's credential. specs/ui-component.md, specs/ui-issued-tokens.md.

    docker compose -f examples/ui/compose.yaml up --build
    open http://localhost:8448                      # alice / demo

No setup step. What the stack cannot commit — the signing key and the worker's credential — is
generated into `./data` on first run and left alone after, so a restart neither loses a run nor
signs anyone out. Delete `./data` to start over.

| user | password | groups (`ui.yaml`) | may |
|---|---|---|---|
| `alice@genroc.test` | `demo` | `genroc-admins` | everything |
| `carol@genroc.test` | `s3cret` | `genroc-operators` | start/pause/retry, not deploy |
| `bob@genroc.test` | `demo` | none — falls to `"*"` | read only |

Measured on this stack:

    user     GET /instances   POST /instances   PUT /definitions
    alice    200              200               200
    carol    200              200               403
    bob      200              403               403

## What happens

**genroc-ui authenticates the person and mints the credential.** It checks the password (or runs
an OIDC login), resolves their groups through `roles:`, and signs a 60-second token carrying the
resulting **permissions**. genroc verifies that signature against a shared secret and reads a
`perms` claim.

    GET /                              no session → the login page
    GET /api/instances  (cookie)       genroc-ui mints a token and attaches it → 200
    GET /api/instances  (Bearer …)     passed through untouched; genroc judges it

**The server has no role map.** It never learns what a group is, which is why the binary carries
no login flow, no cookie handling and no provider config — it is meant to be embedded.

**The browser holds no genroc credential.** Nothing in `localStorage`, nothing to expire. The
session cookie is `HttpOnly`, and the minted token exists only for the duration of one proxied
request.

Everyone still gets attributed. genroc names the caller on every response, and records it on
what they change:

    X-Genroc-Actor: jwt:alice@genroc.test
    hello-script@v2  actor: jwt:alice@genroc.test

## The pieces

    init        generates ./data/{jwt-secret,worker-token} once, then exits
    genroc      the API. Verifies people's JWTs, issues tokens for machines. Port unpublished
    genroc-ui   login, UI, and the proxy that turns a session into a token
    evaluator   the script-task worker; talks to genroc directly with its own token

SQLite, so there is nothing to provision. `-pg <dsn>` is the same image against PostgreSQL;
`examples/auth/` shows that.

**This used to be six services** — Caddy routing on whether a request already carried a
credential, oauth2-proxy running the flow, Dex issuing the tokens. genroc-ui replaced the first
two, and the credential-presence routing went with them: it existed only because one port served
both audiences. specs/ui-component.md §3.

## Adding people

One edit — a `passwords:` entry in `ui.yaml` (`htpasswd -bnBC 10 "" <password> | tr -d ':\n'`)
and its group under `roles:` — then `docker compose restart genroc-ui`. **Nobody is signed out
by that restart**: the signing key lives in `./data`, so every session outlives the process.

`passwords:` is the demo trade — one file, no directory, no registration, no reset, no MFA. A
deployment uses `providers:` instead, and any OIDC provider connects directly: Google, Okta,
Entra, Auth0, Keycloak. One button appears per entry, and with a single provider and no passwords
the chooser is skipped. A provider that is not OIDC (GitHub, LDAP, SAML) needs a broker such as
Dex in front, and genroc-ui's side is unchanged.

**genroc does not change between any of them.** All it ever sees is a `perms` claim signed with
the shared secret.

## Connecting genctl

genctl uses a genroc token; it cannot use your browser session. Sign in as alice, mint one in the
tokens tab, then:

    export GENROC_SERVER=http://localhost:8448
    export GENROC_TOKEN=genroc_sk_...
    genctl apply -f examples/quickstart/hello-script.genroc.yaml \
                 -f examples/quickstart/script-node.genroc.yaml
    genctl run hello-script --set name=genroc      → { "greeting": "hello, genroc" }

genroc's own port is not published, so that goes through genroc-ui, which passes a request that
already carries `Authorization` straight through.

## The evaluator, and the credential a person cannot mint

A worker starts with the stack, before anyone has logged in to mint anything — that is what
`init` and `GENROC_SEED_TOKENS_FILE` are for. It is not that machines are special; boot order is.
The secret is generated into a file rather than passed as an environment variable, which would
put it in `docker inspect` and in every child process.

Its token is `worker` and nothing else:

    POST /external-tasks/claim   200
    GET  /instances              403
    PUT  /definitions            403
    GET  /tokens                 403

It claims parked tasks off genroc's queue — genroc never calls it — so it needs only outbound
access and never touches genroc-ui.

## The part that is actually security

**The guarantee is in the request, not in the network position.** genroc verifies a signature it
can check, with issuer, audience and algorithm pinned and an `exp` required. Reaching it directly
buys nothing, and there is no identity header to forge:

    curl -X PUT -H 'X-Auth-Request-Email: mallory@evil.test' … /api/definitions  → 401

An earlier version of this example ran genroc in `header` mode, where that same request returned
**200 with full admin**. The defence was a strip rule in the router, which genroc could neither
see nor test — a forwarded header and a laundered one are byte-identical on arrival. That is why
the mode is gone. specs/auth-two-credentials.md §1.

**What moved instead is the cookie.** It is now the ambient credential, and ambient credentials
are what CSRF exploits. genroc never sees it — it reads only `Authorization` — so the whole risk
sits in genroc-ui, which is the point of shipping that component rather than wiring up a
general-purpose one: it sets `HttpOnly` and `SameSite=Lax` itself, rather than depending on a
line in someone else's config.

**Two limits worth knowing.** A signed-out session cannot be revoked centrally — `POST
/auth/logout` is the person's own lever, and rotating `./data/jwt-secret` (then restarting both
components together) is the break-glass that signs out everyone at once, leaving `genroc_sk_*`
machine tokens untouched. And groups are captured at login, so a change at an OIDC provider takes
effect at the next one. specs/ui-issued-tokens.md §4.
