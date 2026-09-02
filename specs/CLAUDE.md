# specs/

Specs and design records. **Not every doc here describes shipped behavior** — the ones
listed below are proposals. Do not cite them as current behavior.

`specs/` and `docs/` are divided by what the text asserts, not by polish or audience.
**A spec records a decision** — why a design was chosen, what was rejected, what is still
unsettled — and stays internal. **A doc records behavior** that shipped, in the present
tense, for someone using the system. A spec is not a draft of a doc: nothing is promoted
between them, and when a feature lands its documentation is written against the shipped
behavior while the spec stays put, answering a different question. See
[docs-site.md](docs-site.md).

## Design drafts (proposed, not implemented)

**This heading is approximate and several entries below contradict it** — a doc is listed
here once and then gets built, and the entry says so in its own text rather than moving.
Trust the entry, and the spec's own §0, over this heading. As of 2026-08-25 the ones listed
here that are actually BUILT are `error-extensions` (X2 only), `script-tasks`,
`source-resolution` (code phase), `external-task-queue`, `external-outcome-as-signal`,
`lazy-context`, `object-store`, `compat-command`, `durability-levels` (all but the
per-definition field) and, since 2026-09-02, `api-auth` in full.

- [error-extensions.md](error-extensions.md) — three considered extensions to the child
  error model, of which **X2 (a payload on `raise`) is BUILT (2026-08-22)** — read its §X2-c
  for the decisions and `docs/reference/tasks.mdx` for the behaviour. X1 (batch-shape routing)
  and X3 (opt-in exhaustiveness) remain open questions rather than intended work, each
  recording the case both ways and the signal that should reopen it. X2's own trigger — "grep
  for structured data smuggled into message prose" — is what fired, and the design that
  shipped is caller-declared rather than child-declared, so it cost none of the schema
  machinery the earlier variants were rejected for: `raises` sits where `result_schema` sits,
  and the union-across-patterns machinery it needed already existed. Two things moved during
  the build: the **size cap was dropped** (a process `output` has none either, and the guard
  that does the work is the ergonomics gradient, not a byte count), and the conform that
  narrows an **unknown child output** moved off `engine.collect` onto a catchable
  `output.invalid` — which is why a child task's catchable set is now
  `raises(D) ∪ {output.invalid}` (child-error-handling.md E6).
- [api-auth.md](api-auth.md) — **BUILT, with nothing outstanding**; path layout, permissions and
  `token` mode landed 2026-08-28, `header` mode and the session exchange 2026-09-01, attribution
  (§7) and `jwt` mode 2026-09-02. How genroc gets authenticated at all. The decision everything rests on is
  a split — **genroc owns authorization, the deployment owns identity** — because which
  endpoints a caller may reach is a statement about our own API surface, and pushing it into
  ingress rules means every user keeps a hand-copied list of our routes that goes stale the next
  time `actions.go` grows one. So `Allow` sits on `actionDef` beside `Method` and `Path`, **zero
  value admin-only**, and one `authorize` gate serves every transport — which the transports
  forced, since a middleware on the HTTP mux would have left TCP and UDS open (a unix socket is
  authorized by its file mode instead, the one deliberate exception). **Modes are not
  alternatives**: a proxy authenticates humans and says nothing about how a CI job authenticates,
  so `header` and `token` compose per request, header first — not a preference but the
  observation that a browser has no token and a machine has no forwarded identity, so neither can
  shadow the other. §9 keeps the reasoning that once deferred tokens, because the error is
  instructive: "everyone who needs tokens also has a proxy" is a non-sequitur.

  Four things moved during the build. **Bootstrap grew a fourth path that outranks the other
  three** (§5.3): the operator generates offline with `genctl token generate` and genroc stores
  only hashes, so a secret never originates inside genroc, never reaches its logs and never rests
  in its container — and its auto-mint condition **reversed** from "empty table" to "no live
  admin token", accepting that a revoke-all is undone by a restart, because a deployment holding
  only worker tokens is locked out and the path that exists to let it back in must fire. The race
  the draft predicted was real and its **prescribed fix was insufficient**: `INSERT … WHERE NOT
  EXISTS` does not stop it, because under READ COMMITTED a COUNT takes no lock on rows that do
  not exist — measured at 8 replicas minting 8 admin tokens, and 1 under SERIALIZABLE, which
  SQLite's single writer hides entirely so the test proves nothing without `POSTGRES_DSN`.
  **`header` mode's second failure mode was found by building the example** (§6) and is the
  nastier one: a proxy that FORWARDS a client's copy of the identity header launders a forgery,
  byte-identical on arrival, so `trusted_proxies` cannot see it and only the proxy stripping can
  — the sharpest argument for `jwt` there is. And the **session exchange** (§5.1) settled §10's
  open question in favour of a new endpoint outside `/api/`, minting a real token row rather than
  a signed blob so a person's session is listable, revocable and attributable like any other
  credential; it must never permit a cross-origin read, which is why the UI is served from
  genroc's own origin and no CORS exists anywhere in the system.

  **Attribution (§7) landed last and is the one to read before adding a column anywhere near
  logs.** The actor is ONE value, `source:subject`, because the subject alone cannot say whether
  genroc authenticated an identity or merely wrote down what a proxy asserted — and the
  `none`-mode half the spec promised does work, on the rule that recording is not trusting: an
  asserted header changes no grant, so it needs no `trusted_proxies`, and its source is
  `asserted` rather than `header`. Only operator-initiated rows carry one; the engine advances on
  its own behalf and crediting it to whoever started the run would attribute work nobody asked
  for. The trap it paid for twice is that a `process_logs` column is spelled in FOUR places, and
  the sqlc one (`InsertLog`) serves only the rare object-carrying path while the hand-written
  `writeLogBatch` serves nearly every row — get one and not the other and the feature looks
  entirely dead. internal/db/CLAUDE.md now says so. The other trap is a seam: a **version and a
  pointer want opposite conflict rules** (a definition version is immutable and keeps its first
  deployer; a channel is mutable and takes its last mover), and `ApplyDefinitions` upserts the
  channel OUTSIDE its `Def != nil` block — so the "content already exists, only the pointer
  moves" entry blanked the pointer's actor while still stamping `updated_at`. What the column
  does NOT give is history: it is current state, and a channel's previous mover is gone the
  moment it moves again.

  **`jwt` (§2.1, §2.4) closed the last mode**, at the one dependency it budgeted — golang-jwt for
  the signature, stdlib for the JWKS, since a JWK is base64 and `big.Int` rather than
  cryptography. Its four pins (`iss`, `aud`, the algorithm set, and a REQUIRED `exp`) are parser
  options rather than checks beside the parse, so no path verifies without them, and each is
  refused at config load because every available default accepts more than the operator meant.
  Two findings worth carrying: `jwt` and `token` share the bearer header so they compose in a
  **chain**, where a mode that cannot DECIDE must stop it rather than fall through (an
  unreachable JWKS silently becoming a 401 is an outage no operator can diagnose); and §2.2's
  Google hybrid ships as an **overlay** in the transport, not a mode, because it needs the
  request that `Authenticate` never sees. The `jwks_file` the spec insisted on is what made the
  edge cases testable at all — and testing them revealed that the algorithm pin was NOT actually
  covered by the obvious cases, since `alg: none` and HS256 confusion both fail on key typing
  anyway.
- [ui-component.md](ui-component.md) — **PROPOSAL, nothing built.** Moves the UI out of the
  server into its own image, which also owns the browser login. The server keeps no UI, no OIDC
  flow and no cookie, because it is meant to be EMBEDDED — and the real cost of `-ui` was never
  the handler but the `node` build stage the server image carries to bake `/srv/ui` in. Answers
  api-auth.md §10 ("should genroc run the login flow itself?") with *no, but something we ship
  should*: the flow has to live somewhere, and in the server every embedded deployment would
  carry redirect URIs and cookie handling it has no use for. Keeps §9 literally true (the server
  still never sees a cookie) while closing the gap auth-two-credentials.md §4 left open — that
  `SameSite` guards the browser path from inside oauth2-proxy's config, which is the same
  "security in a file genroc cannot check" that retired `header` mode. Its sharpest structural
  consequence: **the credential-presence matcher disappears rather than moving**, because browsers
  and machines now arrive at different components and nothing has to route on what a request
  carries. Takes Caddy AND oauth2-proxy out of `examples/proxy/`. §5.1 draws the boundary that
  keeps it small: **it relays, it never issues.** Google and every other OIDC provider connect
  directly and cost nothing; GitHub cannot, because with no ID token to forward genroc-ui would
  have to MINT one — a signing key and a JWKS — and an issuer with connectors is Dex, which would
  cost us the very argument §1 of auth-two-credentials rests on. **Embedding Dex** as a library is
  possible and rejected there too: it brings back the signing-key storage that already bit the
  example, puts SAML/LDAP/etcd in the module graph, turns a Dex advisory from an image-tag bump
  into our release — and optimises the exception, since the default deployment is genroc +
  genroc-ui with no Dex at all. Reopen if a single-BINARY deployment (VM, systemd, no
  orchestrator) makes a second process the obstacle rather than a second line of YAML.

- [auth-two-credentials.md](auth-two-credentials.md) — **PROPOSAL, nothing built.** Revises
  api-auth.md's mode set down to two: genroc **issues** opaque `genroc_sk_*` tokens for machines
  and **only verifies** JWTs for people, with no path by which a proxy obtains a genroc token.
  Drops `header` mode on the ground that it is §0's own drift argument turned on authentication —
  its safety is a strip rule in a config genroc cannot see or test — and that api-auth.md §2.2's
  three "no verifiable token" setups are all answered by a broker, since that is a property of
  the PROVIDER rather than the deployment (Dex's GitHub connector even emits `org:team` groups,
  which is more than header mode carried). Drops `/session/token` with it: that exchange solves a
  ROUTING problem, not an identity one, and this design changes the routing — `/api/*` splits on
  **credential presence** rather than by path, so a browser's cookie is turned into a JWT by the
  proxy and the SPA holds no credential at all. The risk it introduces, and the line to get right,
  is that CSRF moves to the proxy's cookie: genroc's "no cookies" rule survives literally since no
  cookie reaches it, and `SameSite` is what closes the gap.

- [custom-tasks.md](custom-tasks.md) — north-star: extend genroc **without plugins** —
  custom tasks are child processes, complex logic lives in an HTTP sidecar they call. Three
  tiers (engine / child process / sidecar), the poller & K8s-handler use cases, and the
  child-as-worker contract (idempotency, cancel, versioning).
- [script-tasks.md](script-tasks.md) — **built** (`eval-node/`, 2026-08-19; moved onto
  `external` + the claim queue 2026-08-24), unlike the rest of this list. Its thesis held:
  running user TypeScript needed **no new engine capability**, and the `external`-plus-worker
  shape it argued for is now what ships — the evaluator claims parked script tasks off the
  queue instead of serving `POST /eval`. Read
  [eval-node/README.md](../eval-node/README.md) for shipped behavior and this doc for the
  decisions. Running user TypeScript needs **no new engine
  capability**: a script task is an `external` task whose input carries a code string, so
  the feature is a setup experience (`create-genroc-app` scaffolds the type generator,
  bundler, tsconfig and worker) rather than a subsystem. The sidecar tier of
  [custom-tasks.md](custom-tasks.md) made turnkey, spending none of its no-plugins
  guarantee — "plugin" here is an optional external component, never loaded code. genroc's
  one addition is a generic `genctl` **import directive**, whose single-pass form is
  **superseded by [source-resolution.md](source-resolution.md)** — what survives is that
  resolution is source-level and client-side, that **the server having no resolver is the
  security answer** (no directive reaches the wire to be tricked into executing), and that
  type checking is the resolver's **exit code** rather than a step anyone adds, so a stored
  definition cannot hold code that failed to typecheck — an ordering property, not an
  enforced rule. Records what the template owns so it does not drift into
  the engine: types checked on the author's machine (schemas already validate both ends at
  runtime, so types were never the safety mechanism — which keeps the *server* free of any
  evaluator dependency), the tsconfig as sandbox, a **pinned rather than deleted**
  clock (retries re-execute; deleting `Date` leaves the generated types asserting what the
  runtime contradicts), and error codes split by honest retryability (a type error and a
  throw are permanent; folding them in with evaluator faults makes the retry budget worse
  than useless). Defers the `process_objects` work until bundles carry libraries — two
  blockers that are one change: definition-embedded values are never externalized, and
  ownership is `(instance_id, hash)` with instance-scoped GC, so code outlives its object;
  routing definition values through the store puts the object under the definition version,
  which *is* the retention rule. Constrained by migration 018's serving rule (unredacted
  context-only objects are never served).
- [source-resolution.md](source-resolution.md) — **code phase built** (2026-08-21;
  `cmd/genctl/sources.go`, `eval-node/import.ts`), structural phase and `$infer` unbuilt.
  How a definition **source file** becomes a definition: a `.genroc` in the repo registers resolver binaries and a
  `"$import: ./x.ts"` directive names one, so a TS bundler, a type generator and a YAML
  fragment loader are all clients of one mechanism. Supersedes script-tasks.md's single-pass
  directive, which cannot work — the `Input` declarations a script typechecks against are the
  *product* of validation's inference — so resolution splits into **phases named by
  permission**: `structural` may change what the typechecker sees and runs before validation,
  `code` may not and runs after it, constrained to **produce a string**, which is what makes
  "cannot invalidate phase 1" enforced rather than promised. Records why the placeholder is
  sound (inference collapses literals to base types today, so
  [literal-types.md](literal-types.md) landing is the signal to re-read), why the two
  roundtrips are a **type query then an apply** rather than a validate then a mutate, and
  that **no server change is required** — `SchemaFile.Tasks[id].Input` is already the
  inferred action-input type. Phase 2 is **batched**, one call per resolver carrying every
  site, because N scripts must not mean N `tsc` runs; `mode: "types"` is the same call
  without the build and is what makes the editor loop work between applies. **genctl passes
  the sites and a resolver never re-detects them** — not for difficulty but for drift, two
  parsers for one syntax. Names the escape the feature exists for and that fails silently if
  skipped (`$` → `$$` on splice, or the Shape layer still reads `${` in the imported code),
  and `$infer` for the other direction — a script's return type extracted into
  `result_schema`, requiring an **explicit annotation** because TS return inference reads the
  parameter type and would otherwise reintroduce the cycle. That makes `$infer`
  [unknown-type.md](unknown-type.md)'s unbuilt **Infer** result-typing mode reached at author
  time, which is also the argument for not scheduling the engine-side one. Closes with why a
  resolver registry is **not the plugin door** [custom-tasks.md](custom-tasks.md) rules out:
  author time, author's machine, ordinary data on the wire.
- [external-task-queue.md](external-task-queue.md) — **BUILT through phase 3** (error channel
  2026-08-23; claim/lease/renew/release and `external.lost` 2026-08-24). Only the long-poll and
  the evaluator switchover remain proposal. Turns `external` into a queue a worker fleet
  **pulls** from. One thing it proposed was **dropped as unsound** on contact with the code:
  splitting `external.timeout` so a never-claimed timeout counts as never-reached and stays
  retryable under `only_once`. The unclaimed path publishes `input` (on the instance detail) and
  answers without a handle (`signal`), so "never claimed" does not prove "never reached", and the
  loosening would break at-most-once for exactly the callers not using the claim API. Foreclosed
  rather than deferred, and **deleting the `GET /external-tasks` listing in 2026-08-28 did not
  change that** — the premise is that an unclaimed path exists at all, which is the approval
  path's whole purpose. Opens by discarding the usual reason for moving
  [`eval-node/`](../eval-node/README.md) off `fetch` — requests are not lost under overload,
  since a failed fetch is a routed code and the instance stays durable; what overload
  produces is a worse *code* (`http.timeout`, unknowable, never retried on `only_once`),
  which is fixable inside the push design. The real case is that a fetch holds one of
  `--max-concurrent` for its duration and an `external` holds none, plus connection
  direction. Its central finding is a schema one: a claim must **not** reuse `worker_id` /
  `lease_expires_at` / `lease_epoch`, which mean "an engine worker is advancing this
  instance" while a claim means the instance is parked — aliasing them locks a worker out of
  its own resolve, delays the `external.timeout` the engine owes at `wake_at`, and forges the
  `ReclaimedExpired` evidence `only_once.interrupted` reads. Separate columns make the task
  deadline stay authoritative over the claim's lease with *no code change*, which is why a
  test must pin it. The error channel routes **through `runExternal` phase 2, not the API
  handler** (retry budgeting needs the lease), and uses the **authored** code namespace a
  child's `raise` already occupies rather than a reserved `external.*` family. Records one
  guarantee the claim makes stronger: `external.timeout` conflates "nobody picked this up"
  with "a worker died mid-execution", and a claim record separates them — hence
  `external.lost`. Phased so the error channel ships first and alone, since the evaluator
  needs it under either shape. Its sharpest correction came from building it: the first cut
  added a third endpoint (`/external-tasks/fail`) and that was wrong in a way only the code
  showed — `/instances/{id}/signal` was left able to report success and not failure, and
  `process_signals` had one `result` column that could not have held the other half, while
  every layer *below* the API had already unified because a result and a failure are one event.
  A submission now carries an **outcome** (`{result}` or `{error}`, discriminated by key
  presence so a null result stays a success) and both addressing modes take either; migration
  027 renames the buffer column to match, which is what lets a failure buffer before the task
  arms. Two further things moved in that build and are marked **[built]** in the doc: the failure payload is conformed **on submission** (a 400 the caller can act on,
  the task left parked) rather than degrading to `output.invalid` the way a child's raise
  must, because the submitter is an HTTP caller and a child's raiser is not; and `raises` is a
  **closed set** on an external task where it is only a payload typing on a child — a child's
  raisable codes come from its own definition so R5 catches a typo at registration, while
  nothing about a worker is knowable until it submits, making submission the only place a
  wrong code can be caught. That closure is what gives `raises: {code: null}` its job — an
  error with no payload still needs a declaration, which reverses the old "null is never a
  declaration" rule on the ground that its premise (omitting already says that) dies with the
  closed set. Null rather than a boolean: it is a schema position, genroc has no boolean
  schemas, and JSON Schema's bare `true` means "any value validates" — the opposite. Null also
  round-trips natively where `true` needed a custom marshaller in each direction. The `only_once` interaction needed no code at all: the existing
  guard already refuses a retry on a worker-reported code at `PUT /definitions` unless the
  rule carries `not_reached: true`.
- [external-outcome-as-signal.md](external-outcome-as-signal.md) — **BUILT (2026-08-24).** An external
  outcome stops being written onto the instance row and becomes a buffered signal like any other;
  the resolve/deliver APIs enqueue and un-park, and `runExternal` phase 2 pops from the queue.
  Both API paths carry two delivery mechanisms today, chosen by a subtle runtime condition
  (`armed && !liveLeased`), and the row-write branch is the store's last non-uniform corner: an
  outcome's references are rooted at `_external.result` while its context key is
  `_external_result`, which is the only `objects` path that does not address the context and
  forces `decodeState` to place references before it lifts the outcomes. The reason that is
  worth spending a change on is the third cost rather than the tidiness: `SetExternalOutcome`
  holds only the row lock and has no reference set to reconcile, so it writes the outcome **uncut
  and undeclared** whatever its size — through the buffer, the engine consumes it under lease and
  writes it through the ordinary `encodeState`. Names the one implementation trap (phase 2 must
  READ in advance and POP in persist; consume-then-yield adds a poll interval to every external
  task, which for the evaluator is every script task) and the behaviour change to state rather
  than discover: FIFO order replaces phase 2a's read-the-failure-first arbitration.

- [lazy-context.md](lazy-context.md) — **BUILT (2026-08-24)** except path-level laziness in
  expressions, which stays proposal. The read side of the object store: one
  `Slot` shape, a `Context` that answers a PATH (`outputs.x.y`) and loads only what that path
  needs, and `Roots` refined from slot names to static path prefixes. Its load-bearing change is
  the smallest one: `resolveNested` stops writing resolved values back through `ContextData`, so
  a value nothing read reaches the next write as the reference it already was -- no load, no
  re-hash. Pass-through *storage* already works (`cutForSize` re-emits an `*ObjectRef` leaf), so
  the blocker was only ever the read path. What shipped needed one thing the doc did not
  anticipate — a **copy-versus-read-through** bit on `Roots`, so a reference the expression only
  copies is never loaded while one it reads into still is (no regression, and none of wish 2's
  machinery). The slot-shape unification it called a prerequisite turned out not to be one and
  was dropped. Records why the end-to-end test could not live in e2e: content addressing makes a
  copied reference and a re-hashed one identical on the wire, so only the in-memory load count
  can see the difference.

- [object-store.md](object-store.md) — **BUILT (2026-08-24)**; only the config-only narrowing of `secret: true` remains proposal. Re-architects `process_objects` from a per-instance blob
  table into a global content-addressed store (`objects`) with explicit ownership
  (`object_refs`: instance / log / definition). Opens with a measurement rather than a design:
  a 221 KB script is copied verbatim into every instance's `external_data` (ten instances =
  2.23 MB) and re-shipped on every claim (670 KB for three tasks, **one** distinct hash), while
  `process_objects` holds zero rows. The four limits it names are shape, not bugs: identity is
  `(instance_id, hash)` so content is per-instance; there is no owner but an instance, which is
  why definition-embedded values are never externalized at all; `pinned` is a boolean that is
  only correct because every write is handed the complete referenced set; and a context pin (a
  reference) and a log horizon (a TTL) are ORed in every predicate, so a third owner would add a
  third clause. Refs collapse both into one rule — an object is collectable when no ref is live.
  Records what the change is most able to break and must be hunted: **a dereference deleting
  content another instance still holds**, deletion being "no live refs remain" and never "my ref
  is gone". Two decisions settled in discussion cut it down further: `owner_kind` governs
  **lifetime, not access**, so reads are one endpoint (`GET /objects/{hash}`) with the content
  address as the whole access rule — knowing a hash is knowing the bytes, so serving by hash
  discloses only **existence**, which is written down as the scheme's honest limit rather than
  claimed away. And **redaction is a display concern, not a boundary**: `secret: true` means "do
  not print this", protecting values at rest is encryption's job, and migration 018's
  unservable-context rule plus `?resolve=true` are retired rather than reimplemented. The
  inconsistency that exposes (inline context redacted, the same value over 2 KiB returned whole)
  is not introduced by the change but *revealed* by it. Records one apparent regression that
  dissolves on inspection and is kept so it is not rediscovered: shared content surviving one
  holder's dereference looks like 018's "a replaced value does not linger" breaking, but the
  property 018 wanted is *no object outlives every claim on it*, which refs preserve exactly —
  the surviving bytes are the other holder's live data, and sharing only ever happens between
  owners that independently produced them. Collection gained a **grace window** on contact with the split read: handing out a
  reference and fetching it are two calls, so the data can move on in between and take the object
  with it — a race `?resolve=true` never had, because materializing was atomic with the read. So
  releasing a claim leaves a `grace` ref (`--object-grace`, default 1h) rather than deleting, the
  GC rule is untouched, and the contract becomes sayable: *a reference you hold is fetchable for
  the window whatever happens to the data*. Only owners stamp grace claims, never the sweep,
  which is what stops an expiring grace earning another window forever. An object on nothing but a grace claim is **unclaimed, not dead** — writing the same
  bytes again claims the row that is already there — and that resurrection races the sweep in a
  way `ON CONFLICT DO NOTHING` causes: it writes nothing, takes no lock, and the collector can
  delete the object between the content upsert and the claim, leaving a dangling ref. The upsert
  must be `DO UPDATE SET size = excluded.size` so it holds the row; SQLite's single writer hides
  the bug and Postgres does not. It retires 018's
  immediate deletion knowingly, and records the price — the window, not the live set, is the
  dominant storage cost for a task that churns a big output in a loop. A **definition claim is
  permanent** (nothing deletes a
  definition version -- verified, not assumed), and needs no branch in the GC: "collectable when
  no ref remains" already means an object with one is never collected, which is the test of
  whether the ref model was the right shape. On the wire, refs leave the data
  entirely for an `objects` section of `{path, ref, size}` with **array** paths (a JSON Pointer
  string would make every recipient implement RFC 6901 unescaping, and is ambiguous between the
  object key "0" and index 0) — the disk format already refuses in-band sentinels and the API had
  reintroduced one. Redaction narrows all the way to the server's **stdout**: the stored trail
  and every API response carry values verbatim, `secret: true` becomes a console-display hint,
  and logs stop being a special case — an externalized log payload is listed like any other
  object and `resolve=true` goes there too. A section belongs to whatever object **owns** its values, so paths are rooted there: the response
  body for an instance detail, each *entry* for a list — `["data"]`, never `["items", 3, "data"]`,
  because a path made of names survives anything a client does while one containing a position is
  valid for a single unmodified page (genctl accumulates pages and reverses rows). The CLI splits
  on what a human wants: `get --resolve`
  splices client-side, `logs` prints object ids and never resolves (a trail is scanned, not read,
  and those payloads are large by definition), and a new `genctl object <ref>` fetches the one
  line that matters. `secret: true` is narrowed to **config only** and refused at
  registration anywhere else, which turned a complication into a deletion: the schema-driven
  redaction of a fetch's response body existed only because the value-based scrub cannot see a
  body that never enters the context, and it was the half that could not move (it collapses value
  to string *before* audit is called, leaving nothing unredacted to store). Config-only removes
  it instead of solving it, and takes the whole taint system with it — `Taint`,
  `ReferencesSecret`, `SecretAt`, `Schema.Redact` propagate secretness through inference purely
  so a redactor can find derived values, and every consumer goes. What is left is one string
  replacement of known config values, on the console copy, in one function.
- [literal-types.md](literal-types.md) — infer `"sent"` as `enum: [sent]` rather than
  `string`. Prerequisite for discriminated unions, and it catches provably-false comparisons.
  The feature is not the 4-line production change but the **enum-aware canonicalization** it
  forces: without it, `?? false` infers an overlapping `oneOf` that rejects `false`. Records
  the number-precision constraint (a literal must keep its exact source text) and that
  `IsSubset` needs no change.
- [discriminated-unions.md](discriminated-unions.md) — **deferred, blocked on literal
  types**, unlike the rest of this list, which is merely unscheduled. Narrowing a `oneOf` by
  a tag check would work against a hand-declared union, but inference does not produce
  literal types (`kind: sent` infers as plain `string`), so a definition cannot build a
  narrowable union and the use case that justifies the feature is unreachable. Read §0 first.
- [guard-narrowing.md](guard-narrowing.md) — refine a value's type from the `switch` case
  that routed to a task, so a definition that proves `x != null` can then use `x`. Records
  two soundness traps that are easy to miss: `config` is re-resolved every tick (so a guard
  on it proves nothing downstream), and task outputs are overwritten on loop re-entry (so
  refinements need a dataflow kill).
- [compat-command.md](compat-command.md) — the compat **check**: two questions, not one.
  Can a running instance continue (**upgrade** — non-negotiable), and does the process still
  honour its contracts (**contract** — excusable with `--ignore contract`). Two shipped
  fixtures report the wrong thing because the two are folded into one word. Records the
  direction rule (**who submits the value**: what they submit may only widen, what we produce
  may only narrow; a verdict only where a conform stands between the parties), that a result
  schema is an *upgrade* concern wherever a task can park mid-flight (external and the child
  family — fetch is the one that cannot), and a three-level report: process, **the schema that
  was compared**, then what `isSubset` said about it. Its sharpest claim is §2e — **a schema
  denotes two different sets**, what may arrive and what is stored once the conform filled its
  defaults, so the same pair in the same direction answers differently; `Validate` already
  distinguishes the cases by mode and `IsSubset` gains the matching one, reading the sub side
  only. An implementation was written and rolled back; findings marked **[run]** came from
  running it, and three contradicted the design as written — chiefly that a slot that changed
  and a value that broke must never share a row, since that claims a cause no comparison can
  know. `config_schema` is deliberately outside the whole check — validation type-checks
  expressions against it, which catches more than compat could — but still gets a
  `(not judged)` row, because a slot missing from the report entirely is how a dropped
  `secret: true` came to be reported nowhere. On §2f — demand pruning — **this entry used to
  say "required, not deferred"; the doc now rejects it outright** and the shipped code has
  none, so read §2f rather than this line: pruning `mustNew(T)` to what is actually read
  would promise an upgrade whose instance then reads a value that is not there.
- [id-list-commands.md](id-list-commands.md) — **BUILT (2026-08-26).** `genctl pause`,
  `resume` and `retry` take several instance ids, iterating client-side like `upgrade`'s id
  form and adding no endpoint. Its premise is that these three verbs **refuse a no-op** by
  design (`db_lifecycle.go:255`, "Report it rather than silently succeeding"), so unlike an
  upgrade sweep a partially applied group does NOT converge on a re-run — which is what
  forces a third outcome, `already`, beside done and refused. What generates the taxonomy is
  a per-verb promise carried by exit 0 (pause: nothing you named is advancing; resume:
  everything is; retry: everything got a fresh attempt), so `completed` counts as `already`
  for pause and as `refused` for resume — the asymmetry is the point, not a wart. Two rules
  keep it honest: classify on the wire error's `code` and never on its prose (genctl
  currently discards the code). It also proposes **the API change that follows from taking
  the assertion model seriously**: `already` is not an error, so `pause`/`resume` return
  a derived status code — `Outcome` on `Reply` beside `Code`, for the same reason `Code`
  lives there (TCP and UDS have no status line), mapping to **200 applied / 202 accepted /
  204 unchanged** — and keep a 409 only where the assertion genuinely cannot hold
  (`resume` on a settled tree), while `retry` — not an assertion — keeps all of its. An
  earlier draft had the CLI re-read the row to classify `resume`'s 409; that the client
  needed a workaround was the signal the API was wrong, and the server decides it under the
  lock that read the tree, which no client can reproduce. Writing it up found a **factual
  error in the spec's own model**: `pause` cannot promise the tree has stopped, because a
  leased row lands in `pausing` and settles only on the worker's next write — 202 is what
  distinguishes asked-and-stopped from asked-and-draining, and a second pause on a draining
  tree must not report `unchanged`. Records why no batch endpoint: five
  pauses are five logical changes, unlike `applyBatch`, which earns its endpoint by being
  one. Two decisions arrived during the build and are recorded as 10 and 11: a malformed
  argument aborts the whole command where a refusal does not (shape-checked up front, so a
  pasted table cannot pause whichever cell parses as a UUID), and `instances` lists **roots
  only** with `-q` for bare ids -- the list a group is fed from must name only what the
  group can act on, since every one of these verbs is root-only. Its **Prior art** section
  is load-bearing rather than decorative: the taxonomy is the
  standard split — an object that does not exist is an error by default (`rm` without `-f`),
  an object already holding the target state is success by default (`systemctl stop` a
  stopped unit) — and `pause`/`resume` are the `systemctl stop`/`start` analogue, which is
  also why `retry` has no counterpart there.
- [durability-levels.md](durability-levels.md) — **one piece built** (`--sqlite-fullfsync`,
  which changes no default); the rest is proposal. Move the fsync off every persist onto a
  few boundaries, exposed as a tunable ladder (`none` → `accepted` → `only-once` →
  `terminal` → `strict`, defaulting to `only-once`). Opens with a measurement, not a
  design: macOS `fsync(2)` does not flush the drive cache, so **every benchmark number
  collected on a Mac is durability-blind** — honest `synchronous=FULL` is 183 inst/s on
  `bench-drain` against the 5,133 the same run reports today, and the 21× is the whole
  motivation. Records why boundaries suffice (prefix durability, stated backwards so
  correctness never depends on what commits *after* the one in question), why the set is
  **ingress rather than egress** (losing egress costs a replay; losing ingress is a
  permanent hang), and why `only_once` cannot be dropped from it — the durable claim is
  the evidence `interruptedOnlyOnce` reads, so without it recovery re-runs the task
  instead of reporting `interrupted`. Also records the engine asymmetry that reorders the
  work: group commit is Postgres-only and costs no durability at all, capped by
  `--pg-max-open-conns` rather than `--max-concurrent`, so the ladder is mostly a SQLite
  feature and should land *after* the two changes that spend no guarantee. **That ordering
  is what was taken (2026-08-25)**: the decision was to keep every commit synchronous and cut
  the NUMBER of flushes rather than their strength, so `--pg-commit-delay` shipped first (§6b)
  — and then the ladder shipped too, on the same principle. `--durability` defaults to
  `only-once`, which flushes what cannot be replayed (work handed in from outside, and
  `only_once` tasks) and lets ordinary task progress replay after a power cut, which the
  at-least-once contract already allows. What is still deferred is the per-definition field. §6a is the honest-storage measurement §8 asked for, and carries two corrections
  worth reading before trusting any fsync number: throughput peaks at a *small* delay and
  falls while the fsync count keeps improving — so "count fsyncs, not time them" is wrong
  for tuning one — and the size of the gain did not reproduce between a loaded and a quiet
  machine (1.33× vs 1.03×), because the knob removes a downside rather than raising a
  ceiling. **Re-measured
  2026-08-25** on the current tree — every figure reproduces (177 / 3,909 / 5,429 inst/s,
  plus a new Postgres row at 2,138 confirming §6's group-commit ratio from `wal_sync`).
  The level is **decided: flag plus a per-definition field**, `max(flag, definition)`, and
  the sub-questions that blocked it are answered in §8 — notably that **child inheritance
  is not a question at all**, since prefix durability means a later sync hardens an earlier
  child's writes whoever wrote them. The §7 `lease_epoch` reuse hazard — the one item that
  had to land before any level below `strict` ships — is **closed**: `worker_id` joined the
  fence predicate, so one epoch is a grant to one worker
  ([lease-fencing.md](lease-fencing.md) records why that is not the `worker_id`-as-token
  option it rejects, and what stays open by choice). The bench-harness bug it named is fixed, and the fix is
  worth knowing — a time window does not scope a shared database, because `dbtest` fixtures
  call `AdvanceClock` and leave rows stamped in the future.
- [deterministic-simulation.md](deterministic-simulation.md) — run the engine against a
  simulated world (virtual clock, modelled fetch service, injected faults) so crashes and
  worker races become enumerable instead of rare. Argues **two tiers and recommends
  building the first**: tier 1 keeps real goroutines and buys fault injection plus a fast
  clock; tier 2 turns every goroutine into an event on one seeded queue, which is what makes
  a run *replay*. The ordering is the point — the oracles, the fake service and the workload
  generator are tier-independent, so the parts needing design thought are built before the
  part needing a refactor that may not pay for itself. The substrate mostly exists and was
  not built for this: `advance()` is already a step function, `db.Now()` is already virtual,
  `--poll 0` already removes the ticker, and `DBTX` is already a decorated interface. §5 is
  the sharpest part and was added after the first draft: the races worth simulating are
  **not goroutine races** but races over database state, and since workers share nothing but
  the database, a **baton at transaction boundaries** (never per statement — an actor would
  block on a row lock still holding it) buys replayable cross-worker interleaving *without*
  tier 2's refactor. That demotes tier 2's remaining prize to intra-worker timing, and §12
  says so against the earlier draft's own recommendation. Carries a recipe table mapping
  each race to the schedule that produces it, PCT for search, and schedule **shrinking** as
  the step people skip before abandoning the simulator — a reproducible failure is still not
  a debuggable one. Its findings landed as ordinary fixes rather than sim work: a shared
  `worker_id` (`hostname-pid`) collapsing the distinction `lease_epoch` rests on, now
  `engine.WithWorkerID`; and `schema.pendingNodes`, a process-global map whose mutex
  synchronised nothing (entries from concurrent solvers are disjoint) and whose missing
  cleanup was a permanent leak. Two blockers remain, both about the clock: it is a **process
  global**, so the frozen-worker incident that motivated
  [lease-fencing.md](lease-fencing.md) — a skew between one worker and the DB — cannot be
  expressed at all, and `AdvanceClock` only moves forward. Records that the `only_once`
  guarantee is checkable **only** here (the oracle is a service that counts its own
  invocations, not anything the DB can be asked), that the fetch timeout must stop being a
  `context` under a simulated transport — inverting the reasoning at
  [action.go:30](../internal/engine/action.go#L30) and leaving two implementations of one
  rule — one oracle that is **not** sound (audit-trail ordering, since buffered log rows are
  best-effort by design and a crash is entitled to drop them), and the crash trap that
  outlives the object graph: **package-level state cannot be dropped**, so a restarted
  worker inherits a warm `template.cache` unless something resets it — which is why
  `internal/archtest`'s allow-list is required to stay small, it *is* the process image.
  Rejects storage-fault accuracy (a `modernc.org/sqlite` VFS) as covering little the `DBTX`
  decorator does not, with `durability-levels.md` named as the signal to reopen.
- [docs-site.md](docs-site.md) — the user-facing documentation site, and the only doc here
  about **user-facing tooling rather than the language**. The gap it fills is *reference*: nothing today
  says what `accepted_status` accepts or what `genctl promote` does. Draws the
  spec-vs-doc line quoted above, and follows it: `docs/` is shipped behavior only, the site
  never links into `specs/`, and the explanation a *user* needs lives in guides rather than
  being outsourced to a spec's argument. Records why plain Astro
  over Hugo (Chroma is vendored, so a genroc lexer is impossible), why no theme (its design
  system is the thing being replaced), Pagefind over any hosted search, and two deployment
  traps — `actions/deploy-pages` would delete the benchmark history on `gh-pages`, and
  GitHub Pages' one-custom-domain-per-repo rule blocks `v1.` subdomains. A playground is
  **not scoped**; it is mentioned only to record that islands are additive, so nothing in
  the architecture anticipates it — plus the one fact that makes it cheap if it ever
  happens: `internal/validation` has no `db`/`engine`/`api` dependency.

## Shipped behavior with a doc here

`pause-resume.md`, `only-once-interrupted.md`, `unknown-type.md`, `delay-syntax.md`,
`recursive-type-inference.md`, `resource-limits.md`, `retry-policy.md`,
`lease-fencing.md`, `typed-values.md`, `map-expressions.md`, `number-precision.md`,
`error-handling-audit.md`, `child-error-handling.md`, `fetch-http-surface.md` describe code
that exists. The invariants extracted from them live in
the `CLAUDE.md` of the owning package.

`child-error-handling.md` is the newest of these: **§5.5 and §12 shipped 2026-08-27**,
reversing D7 so a child task retries like any other and the operator's `retry` re-spawns a
raised child rather than keeping it. Read §5.5 before touching `RetryProcess` or the collect —
it records what breaks silently, including the two defects in the revive walk that the same
work fixed.

`fetch-http-surface.md` is the newest of these and the largest: `query`, the status-keyed
`responses` map that replaced `result_schema` on a fetch, and `self.status` / `self.headers`.
Read it for the decisions rather than the behaviour — `docs/reference/tasks.mdx` is the
present-tense account. Two things it records are not about `fetch` at all and bit elsewhere:
the engine and inference must resolve acceptance through one helper (they diverged once, and
an undeclared 2xx reached `self.result` unvalidated), and the top type may not be read through
a union, a container, a `$ref` or an interpolation — a rule the schema package now enforces in
every access path.

`version-compatibility.md` is now **only** the upgrade half — the gate, the boundary rules,
the tree closure, the one-column write, the endpoints — and it is BUILT (2026-08-26), except
§3b's cross-document pairing check. Everything
that defined the *check* moved to `compat-command.md`, which owns it outright; the two docs
divide at a sharp line (the check reads two documents and never an instance, the gate has the
row in hand). Its §3a prerequisite (dropping `_spawn_result_schema`) shipped, so the address
of a child's `result_schema` in `unknown-type.md` is historical — and that same change is why
a parked parent's `result_schema` is an upgrade concern, not only a contract one.
