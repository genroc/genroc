# API authentication and authorization

Status: **§1, §3, §5 and §6 BUILT 2026-08-28; `header` mode and the session exchange BUILT
2026-09-01.** In place: the path layout (it went first because paths stop being free the moment
a config outside this repo names one), the permission model (`Allow` on every action, one
`authorize` gate every transport passes through, 401/403 as separate codes), **`token` mode**
— `genroc_sk_*` credentials hashed in `api_tokens`, four bootstrap paths, `genroc token` for
break-glass and `genctl token` for everyday use — and **`header` mode**, which with
`/session/token` (§5.1) turns a browser's proxy session into a bearer token. People are now
served as well as machines.

Attribution (§7) followed on 2026-09-02. Still unbuilt: **`jwt` mode** (§2.1) and **TLS** (§9). The default remains
`none` — no `Authorization` handling, no actor recorded, every endpoint open and
`PUT /definitions` arbitrary code execution — now with a startup warning when that is also bound
beyond loopback (§6).

## 0. The split that decides everything

**Genroc owns authorization. The deployment owns identity.**

*Identity* — who this caller is — belongs to the user. They have Okta, Google Workspace, an
ingress controller, a service mesh. Genroc will not out-build any of it and must never become a
user directory.

*Authorization* — which endpoints a caller may reach — cannot be delegated, because it is a
statement about **genroc's own API surface**. Push it into ingress path rules and every user
maintains a hand-copied list of our routes in a file we cannot see: add a route in `actions.go`
and their config does not know, silently opening or closing it depending on their default. The
action registry exists precisely so the endpoint list lives in one place
([internal/api/CLAUDE.md](../internal/api/CLAUDE.md)); an authorization model that lives
somewhere else dissolves that.

So the auth layer's job is to answer **"who is this, and what are they"**. Genroc's job is to
answer **"may that do this"**. A proxy that wants to block paths itself still can — but it must
not have to, and nothing in genroc's design should assume it did.

## 1. Trust zones must be visible in the path

Delegation only works if a rule written once keeps meaning the same thing. That requires a
**documented path contract**: a promise about which prefixes carry which trust zone, tested like
any other contract.

Two zones, and the layout did not express them — both mismatches are now fixed:

- ~~`GET /external-tasks`~~ — the queue listing was operator observability sitting under the
  prefix a worker rule would open. **Resolved by deleting it (2026-08-28)**: it was the polling
  shape `claim` replaced, and everything it published is either derivable from the instance or
  belongs to the claim. external-task-queue.md records the argument.
- ~~`POST /instances/{id}/signal`~~ — the reverse mismatch: low-trust inbound (an external
  system delivering an outcome to a parked task, no claim token) living beside `retry`,
  `upgrade`, `pause` and `resume`. **Moved to `POST /api/external-tasks/signal` (2026-08-28)**,
  taking `instance_id` in the body beside the `task_id` it already carried — so the two delivery
  endpoints sit together and differ only in whether they address by token or by name.

Both are now resolved, and neither by the rename this section originally proposed.

**The layout, as built.**

| zone | paths | who routes it |
|---|---|---|
| **open** | `GET /healthz`, `/public/*` | direct — a probe must never meet a login redirect |
| **inbound** (low trust) | `POST /api/external-tasks/*` — claim, renew, release, resolve, signal | direct |
| **shared** | `GET /api/objects/{ref}` | direct — workers fetch externalized inputs, operators read the same refs |
| **control plane** | the rest of `/api/*` — definitions, instances, channels, tick | direct |
| **human** | everything else — the UI (`-ui`), and `GET /session/token` | through the SSO proxy |

**A `/api/queue/*` prefix was proposed here and dropped.** Its whole justification was that the
operator listing sat under the prefix a worker rule would open; deleting that listing did the
structural work, leaving `/api/external-tasks/*` holding worker verbs and nothing else. Renaming
after that is taste, and it would have cost churn across the evaluator, genctl, the docs and the
tests for no change in what a rule can express.

**Built 2026-08-28**, ahead of the rest of this spec, because it stops being free the moment a
config outside this repo names a path. `apiPrefix` is applied at mount time and the registry
keeps the logical path (`actionDef.mountPath`), so the prefix lives in one constant rather than
28 literals. The OpenAPI spec declares it in `servers`, which is where a base path belongs — a
generated client prepends it, and the documented paths stay the ones the registry routes.
`actionDef.Root` is the exception, and `/healthz` is its only user: a probe must not move when
the API namespace does, so it is mounted at the root and its path item overrides `servers`.

**Everything under `/api/` requires a credential, with no exceptions** — which is the rule a
deployment writes its ingress from, and `TestEveryApiPathIsGated` is what keeps it true. It was
briefly false: the API docs sat at `/api/docs` and `/api/openapi.json` and answered without one,
reading as gated while not being it. They now live under **`/public/`**, so "unauthenticated" is
legible from the prefix rather than from a list of exceptions.

The PER-PROCESS docs stayed under `/api/` and are gated at `read`: they are generated from a
stored definition, so they disclose process names, input schemas and task structure — the
caller's data rather than ours. They cannot be registry actions (they answer with HTML and raw
JSON, not a `Reply`), so `Server.guard` spells the same check at the call site.

**`/healthz` is the one route outside both prefixes**, on the idiom: a probe path is configured
from muscle memory by whoever runs the platform, not by whoever reads this, and `actionDef.Root`
already exists for it. `/process-schema.json` moved INTO `/public/` — an earlier draft kept it at
the root for "parity" with `genroc.org/process-schema.json`, which does not survive examination:
that URL is a released artifact at a stable public address, this is the convenience for an
unreleased build, and getting-started.mdx says which to use. They need not share a path, and one
exception is better than two.

**`/api/` is what lets one hostname serve both audiences** (§5.1). The human path is the
catch-all and machines take the explicit prefix, so a browser arriving at the bare domain lands
on the UI and gets a login flow, rather than the 401 JSON it would get if the API were at the
root. Three prefix rules at the ingress, no regex, no method matching, one DNS name — which was
the point of the exercise, and the reason it was done ahead of the rest of this spec: paths stop
being free the moment a config outside this repo names one.

## 2. Modes: identity in, `Principal` out

One interface, several sources. Every mode produces the same value and nothing downstream knows
which produced it:

```go
type Principal struct {
    Subject string   // who, for the audit trail
    Roles   []string // as asserted by an IdP; empty for a genroc token
    Grants  []Grant  // RESOLVED — the only thing an authorization decision reads
    Source  string   // which mode admitted it — for the audit trail, never for a decision
}
```

`Roles` and `Grants` are separate on purpose. An asserted role is the deployment's word and §4's
map resolves it; a genroc token carries permissions on its row and needs no map. Two paths in,
one field out — so the check in front of every handler has exactly one input and cannot learn
which mode ran. (`Perms []Perm` in the draft; it shipped as `[]Grant` for §3's reason.)

- **`header`** [built] — a trusted proxy authenticated the caller and forwards the result as
  plain headers. Weaker than `jwt` (§6 is the price) but **not legacy**: it is the compatibility
  surface for setups that produce no verifiable token, and there are current, common ones — §2.2.
- **`token`** [built] — genroc's own tokens, hashed in the database, for **machines**: CI,
  deployment pipelines, apps that start instances, and workers. §5.
- **`none`** [built] — the default, and the pre-auth behaviour. Every request is an anonymous
  principal holding `admin`. Right for a laptop and for `make test`; §6 covers the hazard.
- **`jwt`** — unbuilt, and still the mode this design recommends where it is available: a signed
  JWT arrives in `Authorization: Bearer` and genroc verifies it against a configured JWKS. §2.1.

**These are not alternatives — a real deployment runs two at once**, because they serve
audiences that cannot share a mechanism. A browser can do a redirect flow and cannot hold a
secret; a CI job can hold a secret and cannot do a redirect flow.

**How they compose, as built.** `httpPrincipal` tries the forwarded identity first and falls
back to the bearer token. The order is not a preference between them but the observation that
they cannot collide: a browser behind the proxy has no token to send, and a machine bypasses the
proxy and has no forwarded identity, so neither can shadow the other. Trying the header first
also spares the token store a query for a credential the request does not carry.

One consequence is worth stating because it looks like a bug: with `header` mode alone,
`principalFor` returns no principal rather than an anonymous admin. A caller the proxy did not
identify has no second way in — which is the intent, and is also why `/session/token` refuses to
mint against header mode alone (§5.1).

### 2.1 Why the signature, and not the network position

`header` mode is only sound while genroc is unreachable except through the proxy, so its
security rests on a **network fact** — a CIDR allowlist, a NetworkPolicy, a Service that is not
exposed. One `kubectl port-forward` past the ingress and any caller asserts any identity (§6).
That fact is invisible from inside genroc, cannot be tested here, and is exactly the kind of
thing that decays as a cluster is reorganised.

A signed token moves the guarantee into the request. Genroc rejects anything it cannot verify,
so **a bypassed proxy buys an attacker nothing** — the port-forward reaches a server that still
demands a signature it cannot produce. It costs one dependency (`github.com/golang-jwt/jwt/v5`
plus JWKS fetching — genroc's dep list is small and deliberate, so this is a real if modest
addition) and a key to distribute, and it buys the removal of the single worst failure mode in
this design.

Three further gains, none decisive alone: `exp` bounds replay, which a plain header has no way
to express; claims are structured, so there is no per-proxy convention about how a group list is
comma-separated; and verification is offline against a cached JWKS, so there is no per-request
callout the way forward-auth has.

**Most proxies forward rather than mint, and that is the better shape anyway.** oauth2-proxy can
put the IdP's ID token in `Authorization` (`--set-authorization-header`); Istio's
`RequestAuthentication` validates and forwards the original. Genroc then verifies against the
**IdP's** JWKS and there is no second signing key in the system. Cloudflare Access and Pomerium
are the two that mint their own (`Cf-Access-Jwt-Assertion`), which works identically with their
JWKS configured instead. Kong, Traefik and nginx-ingress mostly need a plugin or an Enterprise
tier to do either — which is why `header` mode stays.

### 2.2 Where `header` mode is the only option

`jwt` needs someone to have minted a verifiable token. Three common setups do not, which is why
`header` mode is load-bearing rather than a wart to be removed later:

- **GitHub as the identity provider.** oauth2-proxy's github provider speaks OAuth2, not OIDC —
  there is no ID token to forward. A GitHub-authenticated team has no `jwt` path at all.
- **Google Workspace groups.** The ID token verifies, but Google does not put groups in it;
  oauth2-proxy queries the Directory API and forwards them as a *header*. So Google is a hybrid:
  a verifiable token for identity, a trusted header for roles. The role map must tolerate
  reading its input from a different place than the subject.
- **Service meshes and mTLS.** Identity is a client certificate or a SPIFFE ID; there is no JWT.
  The mesh asserts it as a header, and a future `mtls` mode would read it from the connection
  instead.

The lesson for the implementation: `Principal` must be assemblable from **more than one source
per request** — subject from a verified token, roles from a trusted header — rather than each
mode owning a request outright.

**Built 2026-09-01, and one case turned out to be commoner than "roles from elsewhere".** Two of
the three setups above supply no usable group list at all: oauth2-proxy's GitHub provider is
OAuth2 with no ID token, and Google omits groups unless someone wires the Directory API. A role
map alone has nothing to key on there, so `header` mode also takes a **`users:` map from subject
to permissions** (§4), unioned with whatever the roles produce. It is the degenerate role map —
one member per group — and it is what makes the mode work on the day someone stands up
oauth2-proxy against GitHub with no group plumbing at all.

### 2.3 What the token does NOT decide

A JWT carries **roles**, not permissions. An IdP has no idea what `deploy` means in genroc, and
teaching it would put our authorization model back outside genroc — the thing §0 exists to
prevent. So §4's role map is unchanged by this mode: the token says *who and what group*, the
map says *what that may do*.

The exception is a proxy minting a genroc-specific token, which could carry permissions
directly. Supported by reading them if present, but never required, and never the documented
path: a deployment that puts genroc's permission vocabulary into its proxy config has taken on
the drift problem §0 describes.

### 2.4 Three validations that are not optional

Each is a known way JWT deployments are broken, and none is the default in most libraries:

- **`aud` must be checked.** Without it, a token the IdP minted for a *different* application
  verifies here too — the same signature, a different intended audience. This is the most common
  real-world JWT bug and it turns any other app in the same tenant into a genroc credential.
- **`iss` must be pinned**, so a second, attacker-chosen issuer with a valid JWKS is not
  accepted.
- **The algorithm set must be pinned** to what the issuer actually uses. `alg: none` and
  RS256→HS256 confusion are both live vulnerability classes, and both are configuration, not
  cryptography.

`exp`/`nbf` need a small configurable skew; a fixed zero fails on real clusters.

**A static `jwks_file` must be supported beside `jwks_url`.** It is what makes the mode testable
without standing up an IdP — mint against a fixed key in the suite and hit genroc directly, no
proxy in the loop — and it is also the answer for an air-gapped deployment and for pinning a key
rather than trusting a fetch. A mode that can only be exercised end-to-end is a mode whose
edge cases (`aud` mismatch, expired token, wrong `alg`) never get a test.

## 3. Permissions live on the action registry

**BUILT 2026-08-28.** An `Allow` field on `actionDef`, beside `Method`, `Path` and `Errors`:

```go
{
    Name:   "put_definitions",
    Method: http.MethodPut,
    Path:   "/definitions",
    Allow:  []Perm{PermDeploy},
    ...
}
```

**The zero value is the most restrictive permission**, so an endpoint added without thinking is
closed rather than open. That is the whole reason the field goes here rather than in a table
somewhere: the one place an endpoint is declared is the one place its permission is declared,
and the two cannot drift.

Five permissions, deliberately coarse:

| permission | covers |
|---|---|
| `worker` | the inbound zone — claim, renew, release, resolve, **signal** — and `GET /objects/{ref}` |
| `read` | every `GET`, plus `/definitions/validate` and `/definitions/compat` — analyses that write nothing |
| `operate` | start, pause, resume, retry — acting on *runs* |
| `deploy` | `PUT /definitions`, channels, upgrade — changing *what runs* |
| `admin` | tokens, `/tick`, and anything that declares nothing |

They are a flat set, not a hierarchy: a role maps to a list, and `[read, operate]` says what a
hierarchy would say without inventing an ordering we would then have to defend. `upgrade` is
`deploy` rather than `operate` because it changes which version an instance executes.

**`signal` is `worker`, not `operate` as this table first had it.** §1 moved it into the inbound
zone — it is an external system delivering an outcome to a parked task, not an operator acting
on a run — and the permission has to follow the zone or the path contract says one thing while
the gate does another. `TestWorkerZoneIsExactlyTheInboundEndpoints` is what holds the two
together.

`GET /objects/{ref}` is the only action allowing two permissions, which is the shared zone
expressed as a grant: a worker fetches an externalized input, an operator reads the same ref.
`/tick` is the fail-closed default doing real work — it declares no `Allow` at all and is
admin-only for that reason, not by a decision anyone had to remember to make.

**Two shapes v1 must not foreclose, because both are expensive to retrofit and free now.**

1. **`Perms` is `[]Grant`, not `[]Perm`** — a permission plus an optional, empty-in-v1
   constraint. A bare permission cannot express *"resolve tasks in `approval`"*, which is §9's
   first request after the coarse set works. **Shipped as specified**, constraint declared and
   never populated.
2. **Authorization is two-phase.** A check in front of the handler answers *does this principal
   hold `worker` at all* — but `resolve` carries only a token, and the process it belongs to is
   not known until the row is fetched. So the resource half runs INSIDE the handler, once the
   target is loaded. A pure middleware model cannot express this, and bolting it on later means
   threading the grant into every handler that resolves an id. **Only the coarse half is built**;
   the resource half has nothing to enforce until a constraint can be set.

### 3.1 What the build changed

**`Allow` is a list, not the single `Perm` drafted above**, because several endpoints are
legitimately reachable by two roles and the alternative was the hierarchy this section rejects.
The zero value survives the change and gets sharper: an empty `Allow` is **admin-only**, not
"open", so a forgotten field still fails closed. `TestEveryActionDeclaresAPermission` pins that
each one was a decision rather than an omission.

`Open: true` is the one escape, and `/healthz` is its only user — a probe must answer before an
identity exists. `TestOnlyTheProbeIsOpen` is what stops it becoming two.

**The gate is a function, not middleware**, and the transports forced that: HTTP, TCP and UDS all
dispatch into the registry, so a check installed on the HTTP mux alone would leave two doors
open. `authorize` in `auth.go` is the single call every path makes.

**A unix socket skips the modes entirely**, authorized by its file mode instead — the standard
answer for local IPC, and what the docker socket does. It is the only transport that does: TCP
presents its credential on the envelope's `Token` field, since a stream protocol has no headers.
`principal` on the envelope is unexported precisely so the wire cannot set it directly.

## 4. The role map, and where it lives

Roles are the deployment's words, not ours — `genroc-admins` is whatever their IdP calls it. The
map from those words to permissions is configuration:

```yaml
# -auth-config /etc/genroc/auth.yaml            # as built
mode: header
header:
  subject: X-Auth-Request-Email                 # who
  roles:   X-Auth-Request-Groups                # comma-separated; optional
  trusted_proxies: [10.0.0.0/8]                 # REQUIRED in header mode; §6
roles:
  genroc-admins:    [admin]
  genroc-deployers: [deploy, operate, read]
  oncall:           [operate, read]
  "*":              [read]                      # any authenticated caller
users:                                          # for providers that supply no groups; §2.2
  ada@example.com:  [admin]
session_ttl: 12h                                # bounds a token from /session/token; §5.1
```

`trusted_proxies` accepts a bare address as well as a CIDR: an operator naming one proxy should
not have to know the notation to say so. `session_ttl` refuses zero rather than reading it as
"never" — that is the behaviour the field exists to remove, and spelling it as a duration would
make it look deliberate.

`jwt` is unbuilt and its block is the design, not a shipped schema:

```yaml
mode: jwt
jwt:
  jwks_url: https://accounts.example.com/.well-known/jwks.json
  # jwks_file: /etc/genroc/jwks.json         # alternative: no network, no IdP
  issuer:   https://accounts.example.com     # pinned; §2.4
  audience: genroc                           # pinned; §2.4
  algorithms: [RS256]                        # pinned; §2.4
  subject_claim: email
  roles_claim:   groups
  leeway: 30s
```

**A file, not a table.** The policy governing an API must not be editable *through* that API —
a `deploy` permission that can rewrite the role map is `admin` wearing a disguise. A file
mounted read-only from a ConfigMap is also the k8s-idiomatic and GitOps-shaped answer, and it
needs no bootstrapping story.

`-auth-config` and `-auth token` are **independent flags**, not one setting with several
values, which is the shape §2 argues for: a deployment serving both people and machines passes
both, and each request is admitted by whichever recognises it.

Per-process scoping (`team-a` may deploy `orders-*`) is the obvious next ask and is
deliberately **not** in v1 — §9.

## 5. Machines get tokens, and the proxy has nothing to say about them

A proxy authenticates **humans**. It does nothing for `genctl` in CI, for a pipeline calling
`PUT /definitions`, for an app starting instances, or for a worker pod — and those are most of
how an orchestrator is actually used. This was the reasoning error in an earlier draft, recorded
in §9.

The IdP's `client_credentials` grant is the tidy answer *when it exists*, and often it does not:
Google Workspace uses a different flow, GitHub has no such grant, and Dex — the obvious choice
for a self-contained example — is an identity broker rather than a full OAuth server and does
not implement it. A design that only works on Okta-shaped deployments does not work.

So genroc mints its own. **BUILT 2026-08-28**, as `genroc_sk_` plus 32 random bytes in
unpadded base64url — 43 characters, not the 22 drafted here, because there is no reason to spend
less than a full 256 bits on a credential nobody types:

```
genroc_sk_<43 base64url chars>
```

- **Opaque, not self-encoded.** A random string; the database row carries the permissions. The
  alternative — genroc signing a JWT with the permissions inside — removes a lookup genroc is
  already paying for (every request touches the DB anyway) and buys a problem: revocation then
  needs a denylist, which is the table again, plus a token whose permissions cannot be changed
  without reissuing it. Opaque wins on both counts.
- **Stored as SHA-256, never in the clear.** Unlike a password, a 256-bit random token has no
  guessable structure, so a slow KDF costs latency on every request and buys nothing. Compare in
  constant time.
- **The prefix is load-bearing**: it makes a leaked token greppable in logs and detectable by
  secret scanners.
- **Shown once, at creation.** The row keeps hash, permissions, label, created/last-used, and
  `revoked_at`. Revocation is one row, and it is why an opaque token was the right call.
- `genctl token create --perms deploy --label ci`, `token list`, `token revoke <id>`, and
  `token generate` — which mints **offline**, needing no server and no credential, and is what
  §5.3's fourth path consumes.
- **An unknown permission is refused at mint**, in both `genctl` and `genroc token`. A token
  created with a typo would grant less than asked and the operator would discover it from a 403
  somewhere unrelated.

Bootstrap is §5.3 — it is more than one line, and it is where designs of this shape leak.

### 5.1 One host, split by path — the proxy sits in front of the UI, not the API

**BUILT 2026-09-01**: `-ui` serves the SPA at `/`, `GET /session/token` performs the exchange.

An SSO proxy answers a request carrying no session cookie with a redirect to the login page, so a
script presenting `Authorization: Bearer genroc_sk_…` receives HTML instead of a reply. The two
modes therefore need two **routes** — but not two hostnames, which was an earlier draft's
unnecessary conclusion:

    genroc.example.com/healthz  ->                genroc   probe, unauthenticated
    genroc.example.com/api/*    ->                genroc   machines: Bearer, proxy not in the chain
    genroc.example.com/*        -> oauth2-proxy -> genroc   humans: UI, login flow

Path routing is what an ingress does natively, so this is three prefix rules on one host with the
auth annotation on one of them. DNS stays single and nothing gets a second name.

**The human path is the catch-all on purpose.** With the API at the root instead, a person typing
the bare domain matches the direct route and receives a 401 JSON body rather than a login page —
which is why §1 moves the API under `/api/` rather than leaving it at the root and putting the UI
under `/ui/`.

**The UI still works, because of a decision already made for another reason.** §9 requires the UI
to call the API with a bearer token rather than a cookie, on CSRF grounds. So the page load
traverses the proxy and establishes a session; the SPA then mints a short-lived genroc token from
an endpoint *behind* the proxy and uses it for every API call, which reach genroc directly. The
unification and the CSRF rule are the same decision, and the result is stronger than a two-host
split: **no cookie is ever accepted on the control plane, so it carries no ambient credential.**

**`GET /session/token` is that endpoint, and three things about it are load-bearing:**

- **It lives outside `/api/`.** A deployment routes `/api/*` around the proxy so machine callers
  never meet a login redirect — so a route that needs the proxy's injected identity cannot be
  under it. The browser zone `/*` already goes through the proxy and this falls under that rule
  with no new ingress config.
- **It must never permit a cross-origin read.** It is authenticated by whatever ambient
  credential the browser holds, so a malicious page can *cause* the request; what stops the token
  escaping is that the page cannot read the response. Adding `Access-Control-Allow-Origin` here
  hands every site on the internet a token. This is why the UI is served from genroc's own
  origin (`-ui`): same-origin means no CORS exists anywhere in the system to get wrong.
- **It refuses `header` mode alone.** Minting is pointless if nothing can verify the result on
  the next request — header mode identifies by a forwarded header and a bearer token means
  nothing to it. It answers 501 rather than handing the browser a credential that 401s on every
  call, which is what it did first.

A subject the role map resolves to nothing gets a 403 naming the fix (`add a roles entry for
…`), not an empty token: minting one would produce 403s everywhere with no clue why.

#### Session tokens expire; machine tokens do not

The exchange cannot hand back a token it issued before — only the hash is stored, so the
plaintext is gone the moment it is returned. Every call therefore MINTS, and a browser that asks
on each page load leaves a live credential behind each time. `session_ttl` (default 12h) bounds
them; `expires_at NULL` is what a machine credential keeps, because rotating a worker token is a
deploy, not a clock.

Two rules follow and both are enforced in SQL rather than by callers, for the reason revocation
already is — a check that only some call sites make is the hole that survives review:

- `GetAPITokenByHash` excludes an expired row, so an expired token is indistinguishable from an
  absent one.
- `CountLiveAdminTokens` excludes them too. An expired admin token cannot authenticate, so
  letting it satisfy "a way in still exists" would lock a deployment out permanently the day its
  last admin credential lapsed.

The client half is not optional: a UI that exchanges on every load re-creates the pile-up with a
shorter fuse. `frontend/` asks only when it holds no token, and re-exchanges once on a 401.

Session rows are labelled `session:<subject>`, which is what lets an operator reading
`genctl token list` tell a person's session from a machine's credential.

Rejected: leaving the proxy in the chain for everything and exempting the API with
oauth2-proxy's `--skip-auth-route`. That puts a regex enumerating genroc's paths into the proxy
config — §0's drift problem in miniature — and Go's `regexp` has no negative lookahead, so
"everything except `/ui`" cannot be written and the routes must be listed by hand.

Two consequences worth the arrangement on their own: machine callers stop depending on the proxy
being healthy or correctly configured, and in-cluster workers were always on the API route
whether or not anyone drew it that way.

**On the API route genroc's own auth is the only gate**, which is the intent and also the hazard:
a deployment that publishes it while still in `none` mode has published an unauthenticated
control plane. §6's startup warning is what stands between an operator and that.

### 5.2 Token-only is a supported deployment, not a degraded one

**BUILT, and it is what `examples/auth/` demonstrates.** With no IdP and no proxy at all,
`token` mode covers **100% of the API**: `genctl`, CI, apps and
workers all present `genroc_sk_*`, the permission model is unchanged, and attribution is if
anything better — a token is an identity genroc issued, where a header-borne email is only as
trustworthy as the proxy that set it.

What is given up is SSO and a browser login flow: a person holds a personal token rather than
logging in, and deprovisioning is revoking their tokens rather than disabling one account
upstream. That is the normal shape for infrastructure tools at small scale (Vault, Nomad,
Grafana), and it stops scaling at roughly the size where an organisation already runs an IdP —
which is the point at which `jwt` mode is added *beside* it, not instead of it.

This is the deployment that makes genroc evaluable in ten minutes, so it should stay first in
the documentation. It is also the one that needs TLS in-process (§9), because it is the only
configuration with nothing in front.

### 5.3 Bootstrap: four paths, ranked by root of trust

**BUILT 2026-08-28**, and it grew a path that outranks the three drafted here.

**It is not a first-run problem.** The question is "no usable admin credential exists", which
recurs: enabling `token` mode on a deployment that ran in `none`, or losing the only admin token.
A design that only handles an empty database has no recovery story.

**0. `-seed-tokens` / `GENROC_SEED_TOKENS` — the operator generates, genroc only stores.**
Added during the build and now the recommended path, because it has the best root of trust of
the four: `genctl token generate` mints offline, needing no server and no credential, and genroc
receives `label=perms=secret` entries and stores only their hashes. **A secret therefore never
originates inside genroc, never reaches its logs, and never rests in its container** — which is
the property none of the three below has. The format is deliberately flat, joining perms with
`+`, because it has to survive a compose `environment:` value and a shell; token bodies are
base64url without padding, so they carry no `=` of their own.

Idempotent **by secret, not by label**: re-running is a no-op, and changing a value mints a
second token rather than mutating the first, so rotation is additive and a fleet can roll
without a window where half the workers are refused. An entry whose secret is empty is skipped
rather than rejected, and the skipped label is logged — that is the intended lifecycle for an
admin credential, needed at the first start and then deleted from the file, and naming it makes
a credential that vanished by accident look different from one removed on purpose.

`examples/auth/` is the worked example.

**1. `genroc token create --db …` — a subcommand on the SERVER binary, against the database.**
The root of trust is filesystem access, which is the correct one: anyone who can read the
database already owns every secret in it, so this grants nothing they did not have. No credential
crosses a network or reaches a log, and it is the **break-glass path**, which is why it must
exist even once the others do. Unconditional by construction.

The cost was named in advance and paid: `cmd/genroc` was `flag.Parse()` and nothing else, so
this is what introduced subcommand dispatch to it (`cmd/genroc/token.go`). `genctl` cannot host
it — it speaks HTTP, and bypassing HTTP is the entire point. The secret goes to **stdout** and
everything else to stderr, so `TOKEN=$(genroc token create --perms admin)` yields the credential
alone.

**2. `--bootstrap-token` / `GENROC_BOOTSTRAP_TOKEN` — for automation.** A k8s Secret or a compose
`.env`. Creates the row **only when no usable admin token exists**, ignored otherwise, so it is
idempotent across restarts and doubles as declarative recovery: set the secret, restart, you are
back in. The entropy is the operator's problem; document a generator.

**3. Auto-mint and print — only when neither of the above is set.** To stderr, never to the
audit log, with a line saying the credential is now in the logs and should be rotated. This
exists for `docker run` and for evaluation; it is the weakest of the four because log
aggregation ships it off the box.

**Its condition reversed during the build, from "empty table" to "no live ADMIN token".** The
draft chose empty so that a deliberate revoke-all would not be silently undone at the next
restart. What defeats that is a deployment holding only worker tokens: the table is not empty,
its operators are locked out, and the path that exists to give them a way back in declines to
fire. Expiry counts the same way — an expired admin token cannot authenticate, so letting it
satisfy "a way in still exists" would permanently lock out a deployment the day its last admin
credential lapsed. The cost is the one the draft named and is accepted: revoking every admin
token and restarting mints a fresh one. Recovery is meant to be possible; making it require a
file the operator may no longer be able to reach is how a break-glass path becomes decoration.

**A configured `header` mode suppresses it entirely.** The proxy already identifies an operator
and the role map already gives them admin, so minting an unasked-for credential and printing it
to a log is pure exposure. `genroc token create` remains the break-glass path either way.

**The fleet makes the naive version racy, and a transaction is not the fix.** Genroc runs as
multiple workers against one database, so N replicas start together and all count zero. The
draft prescribed one transaction with `INSERT … WHERE NOT EXISTS` or a unique constraint —
**insufficient, and measured to be**: under Postgres's default READ COMMITTED a `COUNT` takes no
lock on rows that do not exist yet, so every transaction sees zero and every one inserts. Eight
replicas minted eight admin tokens with the plain transaction in place, and one with
`SERIALIZABLE`, which is what shipped (bounded retry, since a loser fails at COMMIT rather than
returning cleanly). **SQLite's single writer hides the entire problem**, which is why
`TestTokens_BootstrapRaceMintsExactlyOne` proves nothing without `POSTGRES_DSN` — the kind of
test that passes everywhere and pins nothing.

**k8s `TokenReview` stays worth building later** — a worker presents its projected ServiceAccount
token, genroc asks the cluster to validate it, and the ServiceAccount maps to `worker`. Nothing
to create, distribute or rotate; the kubelet handles it. Strictly better than a stored token in
k8s, and it needs no new concepts here because it produces the same `Principal`.

## 6. The bypass hazard, stated once and loudly

**This section is about `header` mode only. In `jwt` mode it does not arise** — that is §2.1's
whole argument. `header` shipped first because it is what the common proxies can actually do
(§2.2), which means the hazard below is live for every deployment running it, and `jwt` is worth
building precisely to retire this section.

**There are TWO ways header trust fails, and `trusted_proxies` only covers one.**

The second was found by building the proxy example (2026-08-28) and is the nastier of the pair:
a proxy that **forwards a client's copy** of the identity header launders a forgery into a
trusted assertion. genroc believes it because the peer is the proxy; that the value came from
the client is invisible by the time it arrives. Measured against a running stack, before the
example's config stripped it:

    curl -X PUT -H 'X-Auth-Request-Email: mallory@evil.test' \
                -H 'X-Auth-Request-Groups: genroc-admins' … /api/definitions  → 200

No credential, full admin. So every route the proxy passes through WITHOUT setting the identity
header must delete it — genroc cannot help, because a forwarded header and a laundered one are
byte-identical. That is the sharpest argument for §2.1's signature: a signed token makes the
difference visible, and no proxy configuration can get it wrong on the operator's behalf.

The first way is the one this section was written for:

**Header trust is a total bypass if genroc is reachable directly.** One `kubectl port-forward`
past the ingress and any caller asserts any identity. This is the classic misconfiguration of
this pattern, and the design has to make it hard rather than merely document it. All three
guards are **BUILT 2026-09-01**:

- `trusted_proxies` is **required** in `header` mode — no default, `LoadAuthConfig` refuses to
  start without it.
- A forwarded identity from outside that set yields **no principal**, rather than an error. The
  request may still carry a bearer token another mode accepts, and failing here would break the
  fleet that runs both (§2). It is not "ignored": nothing about the request has been believed.
- In `none` mode bound beyond loopback, one loud warning at startup naming what is exposed. The
  default is `-http :8448` — all interfaces — so `docker run -p 8448:8448` puts an
  unauthenticated `PUT /definitions` on the network. That should be a decision, not an accident.
  Suppressed when `-auth-config` is set, since a proxy is then the answer.

## 7. Attribution is the half that pays for itself

**BUILT 2026-09-02** (migration 038). `process_definitions.actor` answers *"who deployed v7?"*;
`process_logs.actor` answers it for a pause, resume, retry, upgrade and an instance's creation.
Everything written before the migration stays anonymous permanently, which was the argument for
landing it early rather than when it was next asked for.

**One column, holding `source:subject`** — `token:ci`, `header:ada@example.com`,
`none:anonymous`. The source is IN the value rather than beside it because the two facts are only
useful together: `ada@example.com` alone cannot say whether genroc authenticated that identity or
merely wrote down what a proxy asserted, and splitting them into two columns invites exactly the
query that reads the subject and loses the distinction. `Principal.Actor()` is the only place it
is spelled.

**The cheap part does work in `none` mode**, as this section argued. If `-auth-config` names
`header.subject`, genroc records it whatever the mode — and `trusted_proxies` is NOT required for
that, because nothing is being trusted: the grants are unchanged, so a forged header buys exactly
the admin an unauthenticated server already gives everyone. The source is **`asserted`**, never
`header`, so the audit trail distinguishes a value genroc checked came from a trusted peer from
one it merely wrote down. An authenticated principal is never renamed by a header, or a deploy
could be credited to whoever set one.

Three things the build settled that the draft did not raise:

- **Only operator-initiated rows carry an actor.** The engine advances on its own behalf, so
  `AuditCreated` takes the actor for a ROOT instance and `""` for a spawned child, and no engine
  event carries one. Attributing every row the engine then writes to whoever started the run puts
  an identity on work nobody requested.
- **Re-applying identical content does not re-attribute it.** `InsertDefinition`'s conflict path
  leaves `actor` alone, so the first deployer keeps the credit rather than the latest caller
  taking it — and the batch path's "content already exists, only the channel pointer moves"
  entry carries no actor at all, because it writes no definition row.
- **A log column is spelled twice** and the common path is the hand-written one
  (`writeLogBatch`, for buffered rows) rather than sqlc's `InsertLog` (only for rows carrying
  objects). Writing one and not the other fails in the direction that reads as the feature never
  working. Recorded in internal/db/CLAUDE.md because the next column pays it again.

## 8. Two new codes, not one

**BUILT 2026-08-28.** `CodeUnauthenticated` (401) and `CodeForbidden` (403), added to
`errors.go`'s one table.

Collapsing them is tempting and wrong: *"I do not know who you are"* and *"I know, and no"* are
the two most common failures of this feature and they have opposite fixes — one is a broken
proxy wiring, the other a missing role mapping. A single code makes the most frequent support
question undiagnosable from the response.

The 403 body names the permission the action needed, and an empty `Allow` words itself as "the
admin permission" — "requires one of []" tells a reader nothing. Both are what make the
distinction usable rather than merely present.

(The subsection on session expiry that sat here has moved to §5.1, where the exchange it
describes is.)

## 9. Not in scope

- **Genroc as a user directory.** No users, no passwords, no sessions, no password reset. Every
  mode consumes an identity someone else established.
- ~~**TLS in genroc.**~~ **Qualified 2026-08-27.** The original reasoning — *"in k8s it is
  terminated at the ingress essentially always, so a proxy is in the picture regardless"* — is
  true of k8s and false as a general rule. §5.2's token-only deployment is exactly the one with
  no ingress, and there a bearer token crosses the wire in the clear. `--tls-cert` / `--tls-key`
  is roughly ten lines of `ListenAndServeTLS` and is what makes "no proxy" a real configuration
  rather than "no proxy except the one terminating TLS". Certificate *management* — issuance,
  renewal, ACME — stays out: that is what the proxy or the platform is for.
- **Cookies.** The API accepts `Authorization` and configured headers only. A cookie is an
  *ambient* credential: a malicious page makes the browser issue a cross-site request, the
  cookie rides along, and the proxy dutifully forwards the identity header — so header trust
  does not save a cookie-authenticated control plane from CSRF. A UI should exchange its session
  for a short-lived bearer token; the cookie then authenticates only the minting endpoint.

  **Built as prescribed, 2026-09-01.** `GET /session/token` is that endpoint and the only route
  in the system that reads an ambient credential; nothing under `/api/` accepts a cookie. §5.1
  carries the three rules that keep it sound.
- **Scoped grants** — a permission narrowed by a filter rather than held over everything. The
  driver is concrete: a UI that renders forms for one process's approvals should hold something
  that resolves tasks *in that process*, not `worker` over the whole queue.

  **The constraint vocabulary already exists and should be reused verbatim: `(process, version,
  task)`.** That is what `ClaimExternalTasks` already filters on, what the queue index covers,
  and what `process_dependencies` addresses by. A grant of
  `{perm: worker, process: "approval", task: "review"}` introduces no new concept — it is the
  same triple, applied by the server instead of supplied by the caller.

  Nothing new has to be loaded to enforce it. `claim` already turns the triple into SQL
  predicates, so a constrained grant forces them and a caller cannot widen past its own grant;
  `resolve` holds the instance and its current task by the time it validates the token
  (`GetInstance` then `CurrentTask`); `signal` fetches the instance too. What it needs is §3's
  two-phase check, which is why that is called out there rather than here.

  It generalises: `read` or `operate` narrowed to a process works the same way — load the
  instance, compare. One hook, every axis. Not built, because the coarse set is what makes a
  first version reviewable.

  **The shape is reserved rather than merely argued**: `Grant.Constraint` ships declared and
  never populated, so adding a scoped grant changes what the check reads and not the type every
  call site already passes. That was §3's first wish and it cost one struct.

- **A per-TASK grant** is the narrower cousin, and genroc already has one worth not reinventing.
  The two-part
  token `<instance>.<task_epoch>` names exactly one arming: it is validated against the row, it
  stops working the moment the task un-parks or a worker claims it, and a retry moves the epoch
  out from under it. So it is single-use and self-expiring by construction — most of what a
  scoped grant needs, already there.

  What it is *not* is secret-grade. An instance id is a UUIDv7 — around 74 bits of randomness,
  and it appears in logs, CLI history and every instance view. That is fine as a handle passed
  between trusted components and **not** fine as the only thing standing between the public and
  a resolve, which is what a browser form would make it. So the likely shape is a genroc token
  (§5) whose row carries `{perm: worker, instance, task}` plus a TTL — minted per form, revocable,
  attributable — with the external token remaining the addressing INSIDE the request rather than
  the authorization for it. Recording the distinction now because conflating the two is the
  tempting shortcut, and it is the one that puts a weak capability on the open internet.
- ~~**`token` mode first.**~~ **Reversed 2026-08-27, and the original reasoning is kept because
  it is instructive.** It read: *"genroc's own token store is strictly more code than reading a
  header, and every user who needs it in production also has a proxy."* The second clause is a
  non-sequitur — a proxy authenticates humans, and having one says nothing about how a CI job
  authenticates. The question that broke it was "how does an admin generate a token for a
  script?", which has no answer in a proxy-only design. `token` mode is now §5 and ships beside
  `jwt`.

## 10. Open questions

- ~~**Does `none` stay the default?**~~ **Settled 2026-08-28: yes, with a warning.** It
  preserves the pre-auth behaviour and keeps `make test` and the quickstarts working unchanged;
  the alternative — requiring an explicit `-auth=none` — is safer and louder and breaks every
  one of them. What makes it defensible rather than merely convenient is that the danger is
  conditional on exposure, so §6's startup warning fires exactly when `none` stops being a
  laptop default. Revisit if the warning proves ignorable; a warning nobody reads is the same
  as no default at all.
- ~~**Where does the UI's short-lived token come from?**~~ **Settled 2026-09-01: a new
  endpoint.** `GET /session/token`, outside `/api/` so it stays on the proxied route, minting a
  real token row rather than a signed blob — so a session is listable, revocable and
  attributable like any other credential, and `genctl token list` shows it as
  `session:<subject>`. The cookie is never exchanged for anything but this. §5.1.
- **Should genroc run the OIDC login flow itself?** §5.1 unifies onto one host but still needs a
  proxy in front of the browser zone (`/`, where `-ui` serves the SPA). The full unification is genroc implementing the authorization-code
  flow — what Grafana, Argo CD and Gitea all converged on — after which a deployment needs no
  proxy for anything, only TLS (§9). It is a real feature, not a config change, and it is the
  direction the "why is there a second component" instinct points; recorded so it is a decision
  rather than a rediscovery. The cost is that genroc then owns redirect URIs, state/nonce, cookie
  handling and refresh — the surface §9 says it is not in the business of.

  Sharper now that `header` mode ships: a deployment needs the proxy for the **login flow and
  nothing else**, since the session exchange already carries the identity the rest of the way.
  `jwt` would not remove it either — it verifies a token someone else minted. So this remains
  the only thing that would make the proxy optional for a browser, which is both the argument
  for it and the measure of how much surface it buys.
- **Does `Principal.Roles` need to survive into expressions?** A definition that behaves
  differently per caller is a large idea with no demand behind it, and naming it here is enough
  to stop it being added accidentally.
