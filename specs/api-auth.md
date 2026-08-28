# API authentication and authorization

Status: **PROPOSAL 2026-08-27. Nothing built.** Today every endpoint is open: there is no
middleware, no `Authorization` handling, and no actor recorded anywhere. `PUT /definitions` is
arbitrary code execution on the server, reachable by anyone who can open a socket.

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

Two zones, and today's layout does not express them:

- **`GET /external-tasks`** — the queue listing — is operator observability (it exposes evaluated
  task inputs across every process), yet it sits under the prefix a worker rule would open.
- **`POST /instances/{id}/signal`** is the reverse: low-trust inbound (an external system
  delivering an outcome to a parked task, no claim token) living beside `retry`, `upgrade`,
  `pause` and `resume`.

So the two rules a user would write do not separate the two zones, and each mismatch is exactly
the subtle detail we would be asking every user to get right.

**Proposed layout.** Two things at once, because they are one edit: the whole API moves under
`/api/`, and a dedicated prefix inside it carries only worker verbs.

| zone | paths | who routes it |
|---|---|---|
| **open** | `GET /healthz` | direct — a probe must never meet a login redirect |
| **queue** (low trust) | `POST /api/queue/{claim,renew,release,resolve,signal}`, `GET /api/objects/{ref}` | direct |
| **control plane** | the rest of `/api/*` — definitions, instances, channels, `GET /api/external-tasks` | direct |
| **human** | everything else — the UI, `/docs` | through the SSO proxy |

**`/api/` is what lets one hostname serve both audiences** (§5.1). The human path is the
catch-all and machines take the explicit prefix, so a browser arriving at the bare domain lands
on the UI and gets a login flow, rather than the 401 JSON it would get if the API were at the
root. Three prefix rules at the ingress, no regex, one DNS name.

Undecided, and small: `/openapi.json` and `/process-schema.json` are consumed by tooling but also
by browsers. Either side works; they only must not straddle.

`signal` moves to `POST /api/queue/signal` and takes `instance_id` in the body beside the `task_id`
it already carries — the two paths deliver the same `ExternalOutcome` and differ only in whether
they address by token or by name, so one zone is right for both.

This does not re-litigate `queue:` naming, which
[external-task-queue.md](external-task-queue.md) dropped: that was about a field in a
*definition*, where `(process, version, task)` filters are the addressing. This is a URL prefix,
and the concepts do not touch.

**Do this before anyone's ingress depends on it.** Paths are free to change today and expensive
to change once a config outside this repo references them.

## 2. Modes: identity in, `Principal` out

One interface, several sources. Every mode produces the same value and nothing downstream knows
which produced it:

```go
type Principal struct {
    Subject string   // who, for the audit trail
    Roles   []string // as asserted by an IdP; empty for a genroc token
    Perms   []Perm   // RESOLVED — the only thing an authorization decision reads
    Source  string   // which mode admitted it — for the audit trail, never for a decision
}
```

`Roles` and `Perms` are separate on purpose. A JWT carries roles and §4's map resolves them; a
genroc token carries permissions on its row and needs no map. Two paths in, one field out — so
the check in front of every handler has exactly one input and cannot learn which mode ran.

- **`jwt`** — the recommended mode. A signed JWT arrives in `Authorization: Bearer`; genroc
  verifies the signature against a configured JWKS and reads the claims. §2.1.
- **`header`** — a trusted proxy authenticated the caller and forwards the result as plain
  headers. Weaker than `jwt` (§6 is the price) but **not legacy**: it is the compatibility
  surface for setups that produce no verifiable token, and there are current, common ones —
  §2.2.
- **`none`** — the default, and today's behaviour. Every request is an anonymous principal with
  the `admin` role. Right for a laptop and for `make test`; §6 covers the hazard.
- **`token`** — genroc's own tokens, hashed in the database, for **machines**: CI, deployment
  pipelines, apps that start instances, and workers. §5.

**`jwt` and `token` are not alternatives — a real deployment runs both**, because they serve
audiences that cannot share a mechanism. A browser can do a redirect flow and cannot hold a
secret; a CI job can hold a secret and cannot do a redirect flow. Each mode is enabled
independently and a request is admitted by whichever one recognises it.

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

A `Perm` field on `actionDef`, beside `Method`, `Path` and `Errors`:

```go
{
    Name:   "put_definitions",
    Method: http.MethodPut,
    Path:   "/definitions",
    Perm:   PermDeploy,
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
| `worker` | the queue zone, and `GET /objects/{ref}` |
| `read` | every `GET`, plus `/definitions/validate` and `/definitions/compat` — analyses that write nothing |
| `operate` | start, pause, resume, retry, signal — acting on *runs* |
| `deploy` | `PUT /definitions`, channels, upgrade — changing *what runs* |
| `admin` | everything, including `/tick` |

They are a flat set, not a hierarchy: a role maps to a list, and `[read, operate]` says what a
hierarchy would say without inventing an ordering we would then have to defend. `upgrade` is
`deploy` rather than `operate` because it changes which version an instance executes.

## 4. The role map, and where it lives

Roles are the deployment's words, not ours — `genroc-admins` is whatever their IdP calls it. The
map from those words to permissions is configuration:

```yaml
# --auth-config /etc/genroc/auth.yaml
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
roles:
  genroc-admins:    [admin]
  genroc-deployers: [deploy, operate, read]
  oncall:           [operate, read]
  "*":              [read]           # any authenticated caller
```

**A file, not a table.** The policy governing an API must not be editable *through* that API —
a `deploy` permission that can rewrite the role map is `admin` wearing a disguise. A file
mounted read-only from a ConfigMap is also the k8s-idiomatic and GitOps-shaped answer, and it
needs no bootstrapping story.

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

So genroc mints its own:

```
genroc_sk_<22 random chars>
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
- `genctl token create --perms deploy --label ci`, `token list`, `token revoke <id>`.

Bootstrap is §5.3 — it is more than one line, and it is where designs of this shape leak.

### 5.1 One host, split by path — the proxy sits in front of the UI, not the API

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

With no IdP and no proxy at all, `token` mode covers **100% of the API**: `genctl`, CI, apps and
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

### 5.3 Bootstrap: three paths, ranked by root of trust

**It is not a first-run problem.** The question is "no usable admin credential exists", which
recurs: enabling `token` mode on a deployment that ran in `none`, or losing the only admin token.
A design that only handles an empty database has no recovery story.

**1. `genroc token create --db …` — a subcommand on the SERVER binary, against the database.**
The root of trust is filesystem access, which is the correct one: anyone who can read the
database already owns every secret in it, so this grants nothing they did not have. No credential
crosses a network or reaches a log, and it is the **break-glass path**, which is why it must
exist even once the others do. Unconditional by construction.

Cost worth naming: `cmd/genroc` is `flag.Parse()` and nothing else today, so this introduces
subcommand dispatch to a binary that has none. `genctl` cannot host it — it speaks HTTP, and
bypassing HTTP is the entire point.

**2. `--bootstrap-token` / `GENROC_BOOTSTRAP_TOKEN` — for automation.** A k8s Secret or a compose
`.env`. Creates the row **only when no usable admin token exists**, ignored otherwise, so it is
idempotent across restarts and doubles as declarative recovery: set the secret, restart, you are
back in. The entropy is the operator's problem; document a generator.

**3. Auto-mint and print — only when neither of the above is set, and only on an EMPTY table.**
Conditioned on empty rather than on "no usable admin", so a deliberate revoke-all is not silently
undone at the next restart — that recovery belongs to (1) and (2), where it is a decision. To
stderr, never to the audit log, with a line saying the credential is now in the logs and should
be rotated. This exists for `docker run` and for evaluation; it is the weakest of the three
because log aggregation ships it off the box.

**The fleet makes the naive version racy.** Genroc runs as multiple workers against one database
(`RenewWorkerLeases`, `worker_id`), so N replicas starting together each see an empty table and
each mint an admin token — N−1 of them orphaned, unrevoked, and printed into logs nobody reads.
Bootstrap must be one transaction with `INSERT … WHERE NOT EXISTS`, or a unique constraint that
makes the second insert fail rather than succeed.

**k8s `TokenReview` stays worth building later** — a worker presents its projected ServiceAccount
token, genroc asks the cluster to validate it, and the ServiceAccount maps to `worker`. Nothing
to create, distribute or rotate; the kubelet handles it. Strictly better than a stored token in
k8s, and it needs no new concepts here because it produces the same `Principal`.

## 6. The bypass hazard, stated once and loudly

**This section is about `header` mode only. In `jwt` mode it does not arise** — that is §2.1's
whole argument, and the reason `jwt` is the recommended mode rather than the deferred one.

**Header trust is a total bypass if genroc is reachable directly.** One `kubectl port-forward`
past the ingress and any caller asserts any identity. This is the classic misconfiguration of
this pattern and the design must make it hard rather than merely document it:

- `trusted_proxies` is **required** in `header` mode — no default, refuse to start without it.
- A request carrying the identity header from outside that set is rejected, not ignored.
- In `none` mode, bound to a non-loopback address, log one loud warning at startup naming what
  is exposed. The default is `--http :8448` — all interfaces — so `docker run -p 8448:8448` puts
  an unauthenticated `PUT /definitions` on the network. That should be a decision, not an
  accident.

## 7. Attribution is the half that pays for itself

`process_definitions` has no actor column and the audit log has no actor field, so *"who
deployed v7?"* is unanswerable today and stays unanswerable for everything written before this
lands.

The `Principal` fixes that, and **the cheap part works even in `none` mode**: if an identity
header is present, record it, without validating anything. Genroc writes down what the proxy
already decided. `Principal.Source` rides along so a reader can tell an asserted identity from
an authenticated one.

## 8. Two new codes, not one

`CodeUnauthenticated` (401) and `CodeForbidden` (403), added to `errors.go`'s one table.

Collapsing them is tempting and wrong: *"I do not know who you are"* and *"I know, and no"* are
the two most common failures of this feature and they have opposite fixes — one is a broken
proxy wiring, the other a missing role mapping. A single code makes the most frequent support
question undiagnosable from the response.

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
- **Per-process ACLs.** The extension point is the role map — a permission gains an optional
  process pattern. Not built, because nobody has asked and the coarse set is what makes the
  first version reviewable.
- ~~**`token` mode first.**~~ **Reversed 2026-08-27, and the original reasoning is kept because
  it is instructive.** It read: *"genroc's own token store is strictly more code than reading a
  header, and every user who needs it in production also has a proxy."* The second clause is a
  non-sequitur — a proxy authenticates humans, and having one says nothing about how a CI job
  authenticates. The question that broke it was "how does an admin generate a token for a
  script?", which has no answer in a proxy-only design. `token` mode is now §5 and ships beside
  `jwt`.

## 10. Open questions

- **Does `none` stay the default?** It preserves today's behaviour and keeps `make test` and the
  playground working unchanged, at the cost of shipping open-by-default. The alternative — no
  default at all, requiring `--auth-config` or an explicit `--auth=none` — is safer and louder
  and would break every existing quickstart. `jwt` cannot be the default: it needs an issuer
  nobody has configured yet.
- **A UI adds `read` as the scope most of its users should have**, which the set above already
  carries. What it does not settle is where the short-lived token in §9 comes from: a new
  endpoint, or the session cookie exchanged at the proxy.
- **Should genroc run the OIDC login flow itself?** §5.1 unifies onto one host but still needs a
  proxy in front of `/ui`. The full unification is genroc implementing the authorization-code
  flow — what Grafana, Argo CD and Gitea all converged on — after which a deployment needs no
  proxy for anything, only TLS (§9). It is a real feature, not a config change, and it is the
  direction the "why is there a second component" instinct points; recorded so it is a decision
  rather than a rediscovery. The cost is that genroc then owns redirect URIs, state/nonce, cookie
  handling and refresh — the surface §9 says it is not in the business of.
- **Does `Principal.Roles` need to survive into expressions?** A definition that behaves
  differently per caller is a large idea with no demand behind it, and naming it here is enough
  to stop it being added accidentally.
