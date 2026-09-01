# genroc behind an SSO proxy

One hostname, two audiences: people who log in, and machines that present tokens.
specs/api-auth.md §5.1.

    cd frontend && npm install && npm run build      # once
    ./examples/proxy/gen-env.sh                      # once — the evaluator's worker token
    docker compose -f examples/proxy/compose.yaml up --build

    http://localhost:8080     the app

| user               | password  | `auth.yaml` says      | may |
|--------------------|-----------|-----------------------|-----|
| `alice@genroc.test`| `demo`    | `users: [admin]`      | everything |
| `carol@genroc.test`| `s3cret`  | `users: [operate,read]` | start/pause/retry, not deploy |
| `bob@genroc.test`  | `hunter2` | `"*": [read]`         | read only |

Measured on this stack, and the whole point of the arrangement:

    user     GET /instances   POST /instances   PUT /definitions
    alice    200              200               200
    carol    200              200               403
    bob      200              403               403

**The genroc-relevant part of this example is `auth.yaml`, and it is 34 lines.** Everything else
is one way of producing a header; yours will differ.

## What happens when you open it

    GET /                    302 → login → 200
    GET /session/token       alice@example.com → genroc_sk_…
    GET /api/instances       200 with that bearer, 401 without
    PUT /api/definitions     200 for alice, 403 for bob

The browser never holds a genroc credential until it asks for one. oauth2-proxy runs the OIDC
flow and forwards the result as a header; genroc's `auth.yaml` turns the caller's **groups** into
permissions; the session exchange mints a real token row carrying exactly those. From there every
call is an ordinary bearer call — the same path a worker uses.

**`/api/*` skips the login** so a machine presenting a token never meets a redirect, which is why
the exchange lives at `/session/token` and not under `/api/`.

## Adding people

Two edits, and this demo is honest about the second being a cost:

1. `dex/config.yaml` — another `staticPasswords` entry with a bcrypt hash
   (`caddy hash-password --plaintext <pw>`), then restart Dex.
2. `auth.yaml` — a line under `users:`, unless read-only is right, then restart genroc.

**That second edit is the price of `staticPasswords`.** Dex's built-in connector carries no group
membership, so nothing populates `X-Auth-Request-Groups` and the `roles:` map can grant nobody
anything beyond `*`. Every person therefore has to be named individually, in a file read once at
startup.

Swap `staticPasswords` for a `connectors:` entry — LDAP, GitHub, Google, any OIDC provider — and
step 2 disappears: `auth.yaml` names groups, membership lives in the directory, and adding an
operator stops involving genroc at all. That one-line change is commented in `dex/config.yaml`,
and the `roles:` block it needs is already in `auth.yaml`.

It is not only a demo limitation, either: GitHub's provider is OAuth2 rather than OIDC and Google
omits groups from the ID token (§2.4), so real deployments land on the allowlist too.

## Connecting genctl

genctl is a machine client: it goes to `/api/*`, which skips the login, and authenticates with a
**genroc token**. It cannot use your browser session — that lives in a proxy cookie genctl has no
way to hold.

**Mint one in the UI.** Log in at <http://localhost:8080>, open the **tokens** tab, tick the
permissions, name it. The secret is shown once:

    export GENROC_SERVER=http://localhost:8080     # the proxy, not genroc's own port
    export GENROC_TOKEN=genroc_sk_...

    genctl apply -f hello.genroc.yaml
    genctl run hello
    genctl instances

Or persist it, so your environment is not carrying a credential:

    genctl config set server http://localhost:8080
    genctl config set token  genroc_sk_...

Point it at the **proxy**. genroc's own port is not published, and going around the proxy is what
`trusted_proxies` exists to refuse.

**This demo seeds no credential**, and does not need to: the browser path produces an admin
session, and minting is something an admin can do. That is specs/api-auth.md §5 working as
intended — a proxy for people, tokens for machines, and the people can create the machines'.

`genctl token list` then shows both kinds side by side, which is the clearest picture of what the
two halves of this example produce:

    LABEL                      PERMS                EXPIRES         STATUS
    laptop                     deploy,operate,read  -               live    machine: never expires
    session:alice@genroc.test  admin,read           26-08-29 11:11  live    browser: 12h

**Break-glass, if the UI is unreachable** — needs no login and no working API, which is the
correct root of trust for a way back in: whoever can read the database already holds every secret
in it.

    docker compose -f examples/proxy/compose.yaml exec genroc \
      genroc token create --perms admin --label cli \
      -pg 'postgres://genroc:genroc@postgres:5432/genroc?sslmode=disable'

**How a deployment without a browser path does it.** It generates the credential itself and
passes it in, so the secret never originates inside genroc and never reaches its logs:

    genctl token generate                       # any 32+ byte genroc_sk_ value works
    GENROC_SEED_TOKENS='worker=worker=genroc_sk_...' ./genroc ...

Seeding is idempotent and additive — a restart is a no-op, and rotating means adding the new
value and revoking the old, so a fleet rolls without a window where half of it is refused.
`examples/auth` shows that shape, with a script that writes the credentials to a gitignored
`.env`.

## The evaluator, and the credential a person cannot mint

The stack includes the script worker, so there is something to run:

    genctl apply -f examples/proxy/script.genroc.yaml \
                 -f examples/proxy/hello-script.genroc.yaml
    genctl run hello-script --set name=genroc

    Status:  completed
    output:  { "greeting": "hello, genroc", "at": "2026-08-28T21:57:12.233Z" }

genroc parks the `external` task, the evaluator claims it off the queue, runs the TypeScript in
its own realm and answers. genroc never calls the worker — which is why the evaluator needs only
outbound access and no route through the proxy.

### Why this one is seeded and yours is not

A person mints their own credential in the UI. **A worker cannot**: it starts with the stack, no
human present, before anything exists to mint from. That is the whole reason `-seed-tokens` and
`gen-env.sh` are here, and the distinction is worth keeping straight — it is not that machines
are special, it is that boot order is.

`gen-env.sh` generates the value, so the secret never originates inside genroc and never reaches
its logs. Seeding is idempotent and additive: rotating means adding the new value and revoking
the old, so a fleet rolls without a window where half of it is refused.

The token is `worker` and nothing else, which is the weakest permission there is. Measured:

    POST /external-tasks/claim   200      claim, renew, release, resolve
    GET  /instances              403      cannot read what it is running
    PUT  /definitions            403      cannot change what runs
    GET  /tokens                 403      cannot mint another

This is the credential most likely to end up on a machine you trust least, so it is worth scoping
rather than handing it an admin token.

### Two things that will bite you writing a script

**A script is a function body, not a module.** The evaluator builds an `AsyncFunction` with
`input` and `require` in scope, so you `return` — `export default` comes back as a
`compile_error`, which `script.genroc.yaml` routes to a `script_broken` panic.

**Escape `${` as `$${`.** genroc's value layer reads `${...}` as an interpolation of its own
context, so a TypeScript template literal is resolved against the task's input before the script
ever runs. It is caught at apply rather than at run, and the error names the task and the field:

    task "greet" input.code: template expression "name": field "name" not found in schema

## The login page is ours

`dex-theme/styles.css` is the whole design change. Dex loads a theme's stylesheet *after* its
own, so overriding needs no fork of its templates — and an upgrade cannot break markup we do not
own. It follows the OS light/dark setting and uses genroc's own idiom, so the login and the app
do not look like two products.

Two things make this silently not work:

* **Dex embeds its web assets in the binary.** Files placed at `/srv/dex/web` are inert until
  `frontend.dir` points there. Without that line the theme 404s and the stock page renders — no
  error, no log line.
* **A theme directory is not just CSS**; the templates also ask it for `favicon.png` and
  `logo.png`. `Dockerfile.dex` starts the theme as a copy of Dex's `light` so a missing file
  cannot become a broken image.

One trap worth knowing if you edit it: Dex's `main.css` sets `color: #333` on `.dex-container`,
not on the body. Override it on the body alone and dark mode renders near-black text on
near-black — which is exactly what shipped here the first time.

## The part that is actually security

genroc believes a forwarded identity **only from inside `trusted_proxies`**. Reach it directly and
assert the header yourself and you get 401 — without that, header mode is decoration, since anyone
who can open a socket could claim to be anyone. It has no default and genroc refuses to start
without it:

    header.trusted_proxies is required in header mode — without it any caller that
    reaches this port can assert any identity

### The proxy must strip what it does not set

`trusted_proxies` is necessary and not sufficient. genroc believes the identity header because it
arrives from the proxy's address — so a proxy that **forwards a client's copy** of that header
launders a forgery into a trusted assertion. Every route that passes through without setting one
must delete it:

    header_up -X-Auth-Request-Email
    header_up -X-Auth-Request-Groups

Without those, this was measured against the running stack:

    curl -X PUT -H 'X-Auth-Request-Email: mallory@evil.test' \
                -H 'X-Auth-Request-Groups: genroc-admins' … /api/definitions  → 200

No credential, full admin, definition written. With them, 401.

**genroc cannot defend against this**, and that is a property of header mode rather than an
omission: a forwarded header and a laundered one are byte-identical by the time they arrive. It is
the sharpest argument for `jwt` mode, where a signature makes the difference visible (§2.1).

**Set `trusted_proxies` to the proxy, not to localhost.** Here it is the container network, so the
host is outside it. Pointing it at `127.0.0.1` on a single machine would trust every local process.

## The pieces

    Caddy         routes by path, and strips what it does not set
    oauth2-proxy  runs the OIDC flow; answers "is this session valid?" for forward_auth
    Dex           the identity provider: users in dex/config.yaml
    genroc        header mode for people, token mode for machines

Caddy authenticates nothing itself. It asks oauth2-proxy over `forward_auth`, which returns 202
plus the identity headers or 401; on 401 Caddy sends the browser to `/oauth2/start`. That split
keeps the routing and stripping rules in one file and the OIDC flow in the component that
specialises in it.

### Three things that cost an hour each

**An OIDC issuer is one URL resolved from two places.** The browser must reach `localhost:5556`
(login form, redirect); oauth2-proxy must reach `dex:5556` (token exchange, JWKS). Discovery
returns one set and cannot satisfy both, so it is skipped and each endpoint is named from the side
that calls it. Dex's `issuer` stays the browser-facing URL, because it is the `iss` being verified.

**The `groups` scope is not optional once you use a real connector, and it fails silently.** Dex
puts group membership in the token only when the scope asks for it. Without `--scope=... groups`
every caller arrives with no roles and falls through to `"*"` — a 403 that looks like a broken
role map. It is already in `compose.yaml`; `staticPasswords` simply has no groups to send.

**oauth2-proxy sends `approval_prompt=force` by default**, which overrides Dex's
`skipApprovalScreen` — Dex logs *"config skipping approval screen"* and then renders it anyway.
`--approval-prompt=auto` is what actually skips it; empty falls back to the default.

**`--cookie-secret` must decode to 16, 24 or 32 bytes.** `openssl rand -base64 32` fails twice
over: 44 characters, and the standard alphabet is not the urlsafe one oauth2-proxy decodes with.
`openssl rand -hex 16` has neither ambiguity.

## What this trades away

`staticPasswords` buys one container and a user list you can read in the same file you are
configuring. It costs group membership, and therefore the per-person edits above. It also has no
admin UI, no self-service, no password reset, and no MFA.

That is the right trade for a demo and the wrong one for a deployment. When it stops fitting, the
ladder is:

* **A `connectors:` entry** — federate the directory you already have. One line here, and
  `auth.yaml` moves back to `roles:`. This is the intended path.
* **LLDAP + Dex** — if you want a directory but not a heavyweight one. LLDAP is ~1.5 MiB and its
  entire UI is users and groups; Dex's LDAP connector forwards them as a proper array claim.
* **Keycloak** — when you need MFA, self-service registration, password policies or custom
  authentication flows. ~507 MiB and a JVM, and worth it for those.

**genroc does not change between any of them**, which is the point worth taking from this
example: all it ever sees is a header.

## Two modes at once, on purpose

`-auth-config` (header) and `-auth token` are both on. People arrive through the proxy; machines
present tokens directly. Neither shadows the other — a browser has no token to send, and a machine
bypassing the proxy has no forwarded identity.

The exchange **requires** `-auth token`: it mints a bearer token, and without a token
authenticator nothing could verify what it issued. genroc says so rather than handing the browser
a credential that 401s on every call.

Bootstrap is skipped here — header mode already provides an operator path, so genroc does not mint
an admin credential nobody asked for and print it to a log. `genroc token create` remains the
break-glass path.
