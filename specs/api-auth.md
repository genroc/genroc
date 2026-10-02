# API authentication and authorization

Status: **Built**, except the resource half of authorization, scoped grants, k8s `TokenReview`
and attribution history (§10).

Four specs cover auth, each fact in one of them: this one owns authorization, the path contract,
machine tokens, JWT verification and attribution; [auth-two-credentials.md](auth-two-credentials.md)
the rule that genroc accepts exactly two credentials; [ui-component.md](ui-component.md) genroc-ui
and the browser login; [ui-issued-tokens.md](ui-issued-tokens.md) the token genroc-ui mints and
the role map.

## 0. The split that decides everything

**Genroc owns authorization. The deployment owns identity.**

*Identity* — who the caller is — belongs to the deployment's IdP, ingress or mesh, and genroc must
never become a user directory. *Authorization* — which endpoints a caller may reach — cannot be
delegated, because it is a statement about genroc's own API surface: pushed into ingress path
rules, every deployment keeps a hand-copied list of our routes that silently opens or closes a
route the next time `actions.go` grows one. The action registry is the one place endpoints are
declared ([internal/api/CLAUDE.md](../internal/api/CLAUDE.md)), so their permissions are declared
there too (§3).

## 1. Trust zones are visible in the path

A deployment writes its ingress from prefixes, so the prefixes are a tested contract:

| zone | paths | permission |
|---|---|---|
| **open** | `GET /healthz`, `/public/*` (API docs, `openapi.json`, `process-schema.json`) | none |
| **inbound** (low trust) | `POST /api/external-tasks/*` — claim, renew, release, resolve, signal | `worker` |
| **shared** | `GET /api/objects/{ref}` | `worker` or `read` |
| **control plane** | the rest of `/api/*` | §3 |

- **Everything under `/api/` requires a credential**, with no exceptions
  (`TestEveryApiPathIsGated`). What answers without one lives under `/public/`, so the zone is
  legible from the prefix rather than from a list of exceptions.
- **An endpoint goes where its lowest-trust caller needs it.** `signal` is an external system
  delivering an outcome to a parked task, so it sits beside the claim verbs under
  `/api/external-tasks/` and needs `worker`; `TestWorkerZoneIsExactlyTheInboundEndpoints` keeps
  zone and permission saying the same thing.
- `apiPrefix` is applied at mount time (`actionDef.mountPath`) and declared once in the OpenAPI
  `servers`. `actionDef.Root` mounts `/healthz` at the root so a probe does not move with the API
  namespace; its path item overrides `servers`.
- The per-process docs (`/api/definitions/{name}/docs`, `…/openapi.json`) disclose the caller's
  definitions, so they stay under `/api/` at `read`. They answer HTML and raw JSON rather than a
  `Reply`, so they call `Server.guard` instead of being registry actions.
- `/public/process-schema.json` (this build's) and `genroc.org/process-schema.json` (a released
  artifact) differ on purpose.

## 2. Modes: identity in, `Principal` out

```go
type Principal struct {
    Subject string  // who, for the audit trail
    Grants  []Grant // RESOLVED — the only thing an authorization decision reads
    Source  string  // which mode admitted it; for the trail, never for a decision
}
```

Every mode produces this value with `Grants` already resolved — from a token's row, or a JWT's
`perms` claim — so the gate has one input and cannot learn which mode ran.

- **`token`** (`-auth token`) — genroc's own `genroc_sk_*`, for machines. §5.
- **`jwt`** (on when `-jwt-secret-file` or `$GENROC_JWT_SECRET` is set) — an HS256 JWT minted by
  genroc-ui, for people. §2.1, §2.4.
- With neither, no authenticator is installed and every request is `no-auth:anonymous` holding
  `admin` — the default, right for a laptop and `make test`. §6.

The two are independent flags, and a deployment serving people and machines runs both: a browser
can do a redirect flow and cannot hold a secret, a CI job the reverse. Both read
`Authorization: Bearer`, so they compose in a `Chain`: each declines what is not its own (a
`genroc_sk_*` is not three dot-separated segments; a JWT lacks the prefix) and the first to
recognise the credential answers. **An authenticator that cannot decide stops the chain** and the
request gets 503, rather than falling through: an unreachable database read as "not
authenticated" turns an outage into 401s nobody can tell from a bad client.

### 2.1 Why the signature, and not the network position

A signed token carries its own guarantee: genroc rejects what it cannot verify, so a bypassed
proxy or a `kubectl port-forward` buys an attacker nothing — unlike any identity asserted by
network position (auth-two-credentials.md §1). `exp` bounds replay, claims are structured, and
verification is offline. The cost is one dependency (`github.com/golang-jwt/jwt/v5`) and a shared
secret ([ui-issued-tokens.md](ui-issued-tokens.md) §3).

### 2.2 No trusted-header mode

See [auth-two-credentials.md](auth-two-credentials.md) §1.

### 2.3 What the token does NOT decide

The endpoint → permission mapping never leaves `actionDef.Allow`. The token carries only the
result of group → permission resolution, which genroc-ui performs because it is an issuer we
ship, not a third-party IdP (ui-issued-tokens.md §1). `perms` is a fixed claim name, scoped by
the pinned `iss` and `aud`.

### 2.4 Validations that are not optional

Each is a known way JWT deployments break, and all are `jwt.Parser` options rather than checks
beside the parse, so no path verifies without them (`internal/api/jwtauth.go`):

- **`aud` checked** (default `genroc`) — otherwise a token the issuer minted for another
  application verifies here.
- **`iss` pinned** (default `genroc-ui`).
- **The algorithm pinned to HS256**, never read from the token or key, which closes `alg: none`
  and RS256→HS256 confusion by construction. Only a token signed with the right secret and the
  wrong algorithm (HS512) tests the pin: `alg: none` fails on key typing anyway, so it passes with
  `WithValidMethods` deleted.
- **`exp` required**, with a 30s default leeway (`-jwt-leeway`; zero fails on real clusters) — a
  verified token with no expiry is a permanent credential genroc has no way to revoke.

The secret has no default, is refused under 32 characters, and is trimmed when read from a file.
A verified token with no `sub` is refused (401); one granting no recognised permission yields a
principal with no grants (403, since the issuer did authenticate the person).

## 3. Permissions live on the action registry

`Allow []Perm` on `actionDef`, beside `Method` and `Path`. Any listed permission admits, and
`admin` always does. **An empty `Allow` is admin-only**, so an endpoint added without thought is
closed; `TestEveryActionDeclaresAPermission` makes each admin-only action (`/tick`) a named
decision. `Open: true` skips the gate, and `/healthz` is its only user (`TestOnlyTheProbeIsOpen`).

| permission | covers |
|---|---|
| `worker` | the inbound zone — claim, renew, release, resolve, signal — and `GET /objects/{ref}` |
| `read` | every `GET`, plus `/definitions/validate` and `/definitions/compat` — analyses that write nothing |
| `operate` | start, pause, resume, cancel, retry — acting on *runs* |
| `deploy` | definitions, channels, upgrade — changing *what runs* |
| `admin` | tokens, `/tick`, and every other permission |

A flat set, not a hierarchy: a list says what a hierarchy would without an ordering to defend.
`upgrade` is `deploy` because it changes which version an instance executes.

**The gate is a function, not middleware**: `authorize` is called by the HTTP route wrapper and by
`Handlers.Handle` (TCP, UDS), since middleware on the HTTP mux would leave two transports open. TCP
carries its credential in the envelope's `Token` field; a unix socket skips the modes and is
authorized by its file mode, like the docker socket. `Envelope.principal` is unexported so the
wire cannot set it.

**`Grants` is `[]Grant`, not `[]Perm`** — a permission plus a `Constraint` declared and never
populated, because a bare permission cannot express *"resolve tasks in `approval`"*. A scoped grant
(§10) then changes what the check reads, not the type every call site passes.

**Only the coarse half of authorization is built** — *does this principal hold `worker` at all*.
The resource half, inside the handler once the target is loaded, is §10.

## 4. The role map, and where it lives

Not in the server: genroc-ui resolves groups to permissions ([ui-issued-tokens.md](ui-issued-tokens.md)
§5), and policy then cannot be edited through the API it governs. The server takes four flags
describing which tokens to accept — `-jwt-secret-file` (or `$GENROC_JWT_SECRET`, exclusive),
`-jwt-issuer`, `-jwt-audience`, `-jwt-leeway` — each with a `$GENROC_JWT_*` variable; a file for
four scalars would be a parser and a mount for nothing.

## 5. Machines get tokens

People log in through genroc-ui; `genctl` in CI, pipelines, apps and workers — most of how an
orchestrator is used — need a credential of their own. Not the IdP's `client_credentials` grant:
Google Workspace, GitHub and Dex do not implement it. So genroc mints `genroc_sk_` plus 32
random bytes, unpadded base64url (43 characters):

- **Opaque, not self-encoded.** The row carries the permissions. A genroc-signed JWT would save
  a lookup genroc already pays for and need a denylist — the table again — to revoke.
- **Stored as SHA-256**, compared in constant time. A slow KDF would add latency to every request
  and buy nothing against 256 random bits.
- **The prefix is load-bearing**: a leaked token is greppable in logs and detectable by secret
  scanners, and it is hashed as part of the token.
- **Shown once, at creation.** The row keeps hash, permissions, label, created / last used,
  `revoked_at`, `expires_at` (unused — auth-two-credentials.md §6), `actor` and `revoked_by`.
- `genctl token create --perms deploy --label ci`, `list`, `revoke <id>`, and `generate`, which
  mints offline with no server and no credential (§5.3, path 0). `POST`/`GET /api/tokens` and
  `DELETE /api/tokens/{id}` are admin-only.
- **An unknown permission is refused at mint** by the API, `genroc token` and `-seed-tokens`, all
  through `api.ValidPerms`: a typo would grant less than asked, discovered as a 403 somewhere unrelated.

### 5.1 One host: browsers through genroc-ui, machines direct

The API under `/api/` with the UI as the catch-all lets one hostname serve both audiences: an
ingress sends `/api/*`, `/healthz` and `/public/*` straight to genroc and everything else to
genroc-ui, so a person typing the bare domain lands on a login rather than a 401 JSON body, and no
script ever meets a login redirect. genroc-ui also proxies those prefixes itself
([ui-component.md](ui-component.md) §3), so it can stand alone in front.

**On the direct route genroc's own auth is the only gate**: a deployment publishing it in the
default mode has published an unauthenticated control plane, which §6's warning exists for.

### 5.2 Token-only is a supported deployment, not a degraded one

With no IdP and no login configured in genroc-ui, `token` covers the whole API: `genctl`, CI, apps
and workers present `genroc_sk_*`, and a person pastes one into the UI. It is what
`examples/auth/` runs. What is given up is SSO — deprovisioning is revoking tokens — and `jwt` is
added beside it, not instead, once an organisation runs an IdP.

### 5.3 Bootstrap: four paths, ranked by root of trust

**The problem is "no live admin token", not first run**: it recurs when `token` is enabled on a
deployment that ran without auth, or when the last admin token is lost.

0. **`-seed-tokens` / `-seed-tokens-file` / `$GENROC_SEED_TOKENS` — the operator generates,
   genroc only stores.** `genctl token generate` mints offline, and genroc receives
   `label=perms=secret` entries (perms `+`-joined, to survive a compose `environment:` value and
   a shell) and stores their hashes. A secret never originates inside genroc, reaches its logs,
   or rests in its container — the recommended path. Idempotent **by secret, not label**:
   changing a value mints a second token, so rotation is additive and a fleet rolls without
   refusals. An entry with an empty secret is skipped and its label logged, so an admin
   credential deleted from the file after first start reads as deliberate. A secret a revoked
   token holds is skipped with a warning naming its label: revocation survives a restart.
1. **`genroc token create|list|revoke --db …` — on the server binary, against the database.** The
   root of trust is filesystem access, which already owns every secret in it, so this is the
   break-glass path and unconditional. `genctl` cannot host it: it speaks HTTP, and bypassing
   HTTP is the point. The secret goes to stdout and everything else to stderr, so
   `TOKEN=$(genroc token create --perms admin)` captures the credential alone.
2. **`-bootstrap-token` / `$GENROC_BOOTSTRAP_TOKEN`.** Created only when no live admin token
   exists, so it is idempotent across restarts and doubles as declarative recovery. A secret a
   revoked token holds then fails startup saying so; it is never reinstated.
3. **Auto-mint**, when 2 is unset and no live admin token exists: printed once to stderr with a
   line saying to rotate it. The weakest, because log aggregation ships it off the box. Skipped
   when jwt mode is on, since genroc-ui is then the operator's way in.

**"No live admin token", not "empty table"**: a deployment holding only worker tokens would
otherwise be locked out with the recovery path refusing to fire. An expired admin token counts as
absent for the same reason. Accepted cost: revoking every admin token and restarting mints a new
one.

**The check runs under SERIALIZABLE** (`EnsureBootstrapToken`, with bounded retry since a loser
fails at COMMIT). Under READ COMMITTED a `COUNT` locks no rows that do not exist yet, so N
replicas starting together all insert — 8 replicas minted 8 admin tokens with a plain
transaction, 1 under SERIALIZABLE. SQLite's single writer hides this, so
`TestTokens_BootstrapRaceMintsExactlyOne` pins nothing without `POSTGRES_DSN`.

## 6. The exposure warning

No-auth stays the default so `make test` and the quickstarts run unchanged, and is made
defensible by a startup warning: with no authenticator and `-http` bound beyond loopback (the
default `:8448` is all interfaces), genroc logs that anyone reaching the port can register a
definition — arbitrary code execution. Suppressed when jwt mode is on. If the warning proves
ignorable, the default should change.

## 7. Attribution

Who did it is recorded as one value, `actor`, on:

- `process_definitions` — who deployed a version;
- `process_channels` (with `updated_at`) — who last moved a pointer, and when;
- `process_logs` — an operator's pause, resume, cancel, retry, upgrade, and an instance's creation;
- `api_tokens` — `actor` minted it and `revoked_by` killed it. Two events, so two columns: a
  single column would silently change meaning on revocation. Minting is the one write that grants
  access and has no audit-log fallback, since a token belongs to no instance.

**The value is `source:subject`** — `token:ci`, `jwt:ada@example.com`, `no-auth:anonymous`,
`engine:self`, and for mints outside any request `startup:seed-tokens`,
`startup:bootstrap-token`, `startup:auto-mint`, `cli:token-create` (`cli:token-revoke` on a
revocation). The source is in the value because the subject alone cannot say how the identity was
established, and two columns invite the query that reads one and loses the other.
`Principal.Actor()` is the one place it is spelled. `no-auth`, not `none`: beside `startup:` and
`cli:`, `none:` reads as a missing value.

- **The engine names itself.** Its own advances are `engine:self` (`model.ActorEngine`, applied in
  one place, `audit`), never the operator who started the run. `AuditCreated` takes the caller's
  actor for a root instance and none — so `engine:self` — for a spawned child.
- **`actor` is non-empty on every row genroc writes**; empty means the row predates attribution.
  It is `NOT NULL DEFAULT ''` everywhere and never nullable in Go, and nothing backfills it.
- **A version and a pointer take opposite conflict rules.** A definition version is immutable, so
  `InsertDefinition`'s conflict path keeps its first deployer; a channel is mutable, so
  `UpsertChannel` takes the last mover. `ApplyDefinitions` upserts the channel outside its
  `Def != nil` block, so the pointer-only entry must carry the actor too — `actor` and
  `updated_at` describe one write (internal/api/CLAUDE.md).
- **A log column is written in two places** — `writeLogBatch` for buffered rows, the common path,
  and sqlc's `InsertLog` for rows carrying objects (internal/db/CLAUDE.md).

**No history**: a channel's actor is its last mover (§10).

## 8. Two codes, not one

`CodeUnauthenticated` (401) and `CodeForbidden` (403): *"I do not know who you are"* and *"I
know, and no"* have opposite fixes — a missing credential versus a missing permission — and one
code makes the commonest support question undiagnosable. The 403 body names the permission the
action needed, and an empty `Allow` words itself as "the admin permission".

## 9. Not in scope

- **Genroc as a user directory.** No users, no passwords, no sessions, no password reset in the
  server. Every mode consumes an identity someone else established.
- **TLS.** Terminated in front of genroc, as for any service.
- **Cookies.** genroc reads `Authorization` only. A cookie is an *ambient* credential, which is
  what CSRF exploits; the browser's cookie belongs to genroc-ui, which sets `SameSite=Lax` itself
  ([ui-component.md](ui-component.md) §2).
- **The caller's `Principal` in expressions.** A definition that behaves per caller is a large
  idea with no demand behind it.

## 10. Open

- **The resource half of two-phase authorization.** `resolve` carries only a token, so its
  process is unknown until the row is fetched: the check runs inside the handler, once the target
  is loaded. Nothing to enforce until a scoped grant exists; built with it.
- **Scoped grants** — when a UI rendering one process's approvals should hold `worker` over that
  process, not the whole queue. Reuse the `(process, version, task)` triple verbatim — what
  `ClaimExternalTasks` filters on and `Grant.Constraint` declares. Needs the resource half above.
- **Per-task grant.** The external token `<instance>.<task_epoch>` is addressing, not
  authorization: an instance id carries no randomness. A browser form needs a minted single-task
  genroc token (§5) carrying `{worker, instance, task}` and a TTL.
- **k8s `TokenReview`** — when workers run in k8s: the cluster validates a projected
  ServiceAccount token, which maps to `worker` — the same `Principal`, nothing to distribute.
- **Attribution history** — when "who promoted v7 to prod" must outlive v8. Needs an audit table
  not keyed by an instance: `process_logs.instance_id` is NOT NULL.
