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

**The genroc-relevant part is `auth.yaml`, and it is 34 lines.** Everything else is one way of
producing a header; yours will differ.

## What happens

    GET /                    302 → login → 200
    GET /session/token       alice@genroc.test → genroc_sk_…
    GET /api/instances       200 with that bearer, 401 without

oauth2-proxy runs the OIDC flow and forwards the result as a header; `auth.yaml` turns that
identity into permissions; the exchange mints a real token row carrying exactly those. From
there every call is an ordinary bearer call — the same path a worker uses.

`/api/*` skips the login, so a machine never meets a redirect. That is why the exchange lives at
`/session/token` rather than under `/api/`.

## Adding people

Two edits: a `staticPasswords` entry in `dex/config.yaml` (bcrypt: `caddy hash-password`), then
a line under `users:` in `auth.yaml`. Both need a restart.

**The second edit is the price of `staticPasswords`**, which carries no group membership — so
nothing populates `X-Auth-Request-Groups` and `roles:` can grant nobody anything beyond `*`.
Swap it for a `connectors:` entry (commented in `dex/config.yaml`) and that edit disappears:
`auth.yaml` names groups, membership lives in the directory.

Not only a demo limit — GitHub is OAuth2 rather than OIDC and Google omits groups from the ID
token (§2.4), so real deployments land on the allowlist too.

## Connecting genctl

genctl uses a genroc token; it cannot use your browser session, which lives in a proxy cookie.
**Mint one in the UI** — log in, tokens tab, tick permissions, name it:

    export GENROC_SERVER=http://localhost:8080     # the proxy, not genroc's own port
    export GENROC_TOKEN=genroc_sk_...

Nothing is seeded for people: the browser path produces an admin session and minting is an admin
action. Break-glass, if the UI is unreachable:

    docker compose -f examples/proxy/compose.yaml exec genroc \
      genroc token create --perms admin -pg 'postgres://genroc:genroc@postgres:5432/genroc?sslmode=disable'

## The evaluator, and the credential a person cannot mint

    genctl apply -f examples/proxy/script.genroc.yaml -f examples/proxy/hello-script.genroc.yaml
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

genroc believes a forwarded identity **only from inside `trusted_proxies`**. Without it, anyone
who reaches the port asserts any identity; genroc refuses to start rather than default it.

### The proxy must strip what it does not set

`trusted_proxies` is necessary and not sufficient. genroc believes the header because it arrives
from the proxy's address, so a proxy that **forwards a client's copy** launders a forgery into a
trusted assertion. Measured before the strip existed:

    curl -X PUT -H 'X-Auth-Request-Email: mallory@evil.test' … /api/definitions  → 200

**genroc cannot defend against this** — a forwarded header and a laundered one are byte-identical
on arrival. It is the sharpest argument for `jwt` mode (§2.1), where a signature makes the
difference visible.

## The pieces

    Caddy         routes by path, strips what it does not set
    oauth2-proxy  runs the OIDC flow; answers forward_auth
    Dex           the identity provider, users in dex/config.yaml
    genroc        header mode for people, token mode for machines

Caddy authenticates nothing itself — it asks oauth2-proxy, which returns 202 with identity
headers or 401. That keeps routing and stripping in one file and OIDC in the component that
specialises in it.

### Three things that cost an hour each

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

**genroc does not change between any of them** — all it ever sees is a header.
