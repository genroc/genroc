# The object store: content, and who holds it

Status: **BUILT 2026-08-24** — the store, the wire, definition objects and worker caching. What
remains proposal is the config-only narrowing of `secret: true` (§Redaction), which is mostly
deletion. Re-architecting `process_objects` from a
per-instance blob table into a global content-addressed store with explicit ownership. The
trigger is script tasks — a bundle that carries a library is copied into every instance and
re-shipped on every claim — but the fix is the ownership model, not a special case for code.

## The measurement

A 221 KB script, ten instances, nothing else running:

| | |
|---|---|
| definition row | 223 KB — one copy, correct |
| ten instances' `external_data` | **2,233,580 bytes** — the script, verbatim, ten times |
| `process_objects` | **0 rows** |
| claiming three tasks | **670,686 bytes**, **one** distinct content hash |

Both costs are linear in something unbounded: instances, and claims. Ten thousand instances of
that script is 2.2 GB of duplicated code on the hot table.

## What is wrong with the current model

`process_objects(instance_id, hash)` holds content, `pinned` (0/1) and `log_until` (a
millisecond horizon, NULL = no log needs it). An object lives while `pinned = 1` **or** the
horizon has not passed. Migration 018 argues that carefully and it is internally consistent —
these are not bugs, they are limits of the shape:

1. **Identity is per-instance, so content is per-instance.** The primary key is
   `(instance_id, hash)`. Byte-identical content in ten instances is ten rows. Content
   addressing dedups *within* an instance and nowhere else, which is the measurement above.
2. **There is no owner but an instance.** Code comes from a definition and lives as long as the
   definition version. There is nowhere to say that, so definition-embedded values are never
   externalized at all — they are re-evaluated into every instance's `external_data`.
3. **Ownership is implicit and recomputed.** `pinned` is a boolean, not a count, so it is only
   correct because `applyContextObjectDiff` is handed the *complete* set of hashes the instance
   still references and diffs against what it loaded. That works, but it means no write may ever
   know less than the whole picture, and nothing can answer "who holds this row".
4. **Two lifetimes, two mechanisms.** A context pin is a reference; a log horizon is a TTL.
   They are ORed in every predicate (`pinned = 0 AND (log_until IS NULL OR log_until < ?)`),
   and a third owner would add a third column and a third clause.

## The shape

Split content from the claims on it.

    objects(hash PK, content, size, created_at)

    object_refs(hash, owner_kind, owner_id, expires_at NULL,
                PRIMARY KEY (hash, owner_kind, owner_id))

`owner_kind` is `instance` (a live context value-slot), `log` (a log payload) or
`definition` (a value embedded in a definition version, `owner_id` = `name@version`).
`expires_at` is the log's retention horizon; NULL means the ref lives until it is removed,
which is what a context pin and a definition ref are.

A ref is **live** when `expires_at IS NULL OR expires_at >= now`. An object is collectable when
it has no live ref. That is the whole GC rule, and it replaces both the `pinned` boolean and
the `log_until` column — the two lifetimes become two rows rather than two mechanisms, and the
third owner needs no new clause.

**Content is stored once, globally.** That is the point: the hash IS the identity, as content
addressing always meant. Ten instances referencing one script are ten ref rows and one object.

## What this must not break

- **A dereference must not delete content someone else holds.** Today deleting is trivially
  safe because rows are per-instance. Under a shared store the same delete can destroy an
  unrelated instance's value, which is the failure this design is most able to cause and the
  one the tests must hunt. Deletion is only ever "no live refs remain", never "my ref is gone".
- **Reads are addressed by content, and that is the whole access rule.** `GET /objects/{hash}`,
  one endpoint, no owner scoping. The address IS the content: knowing a hash is knowing the
  bytes that produce it, so serving by hash discloses nothing a holder of the hash did not
  already have. What it does disclose is **existence** — that some instance somewhere holds
  exactly these bytes — which is the honest limit of the scheme and worth writing down rather
  than claiming unguessability. It bites only for content an attacker can reconstruct
  byte-for-byte, and an object exists only above 2 KiB.
- **`owner_kind` governs lifetime, not access.** That separation is what makes one endpoint
  possible: refs answer "how long does this live", the hash answers "may I read it". Nothing in
  the read path consults a ref.

## Redaction is a recording concern, not a read concern [decided 2026-08-24]

Migration 018 kept unredacted context objects unservable, and `?resolve=true` materialized then
redacted. Both go, and the reasoning behind them is retired rather than reimplemented:

**`secret: true` means "do not record this", not "do not return this".** Its job is log
hygiene — the place a value is read by a human who did not ask for it. Protecting values *at
rest* is encryption's job, and encrypting objects is the intended next layer; redacting on read
was never that, and pretending it was is what made the API grow two shapes for one concept.

So redaction survives in exactly one place: **the server's own stdout**, where a value is read
by an operator who did not ask for it. Everything else — the stored trail, every API response —
returns what actually happened.

`audit()` currently redacts *before* it splits, so one pass protects both sinks:

```go
if secrets := e.contextSecrets(inst); len(secrets) > 0 {
    ev.Data = redactSecrets(ev.Data, secrets)   // ← applies to BOTH
    ...
}
consoleEv := ev
consoleEv.Data = truncateStr(ev.Data, e.payloadCap())
e.emit(consoleEv)                                // stdout
e.db.AppendLog(...)                              // process_logs, and thus the API
```

The scrub moves inside the console branch; the stored entry keeps the value verbatim, and
`api/handlers_instances.go`'s `RedactContext` goes entirely.

### `secret: true` is CONFIG-ONLY [decided 2026-08-24]

The marker is valid in `config_schema` and **refused at registration anywhere else** — not
ignored, refused, so a definition that expects protection is told it will not get it. Config is
where secrets enter a process; the rest was generality nobody was buying.

This started as a complication and became a deletion. There are two redaction mechanisms today:

- **(a) `redactSecrets`** — string replacement of known secret *values* over the rendered log
  text. It is airtight for the reason its own comment gives: expressions have no functions, so a
  secret always appears verbatim in any logged value.
- **(b) `ResultRedactionSchema` + `sc.Redact(body)`** — structural, walking the value and
  blanking `secret: true` fields. It exists *only* because (a) is blind to a fetch's response
  body: (a) learns values from the instance context, and a response body never enters the
  context — only the projected output does.

(b) was the hard one to move, because it runs at the call site inside `snippetResult`, collapsing
value to string *before* `audit()` is called — so audit has no unredacted version left to store.
Config-only removes (b) outright rather than solving it: with no `secret: true` in `responses`,
there is nothing for it to blank.

What that leaves is one mechanism, in one place, on one sink: `redactSecrets` over the console
copy, with `contextSecrets` collapsing to `def.SecretConfigValues(inst.Config)` — no schema walk
at all.

**The larger prize is the taint system.** `Taint()`, `ReferencesSecret`, `SecretAt`,
`pathHitsSecret`, `nodeOrTargetSecret` and `Schema.Redact` exist to propagate secretness through
inference so that a redactor can find derived values later. Their only consumers are the three
redaction paths above, every one of which goes — leaving `IsSecret()` on a config property, used
for coercion and for collecting the values to replace. Verify that during implementation rather
than trusting this paragraph, but the shape of the change is a deletion, not a refactor.

**The consequence, stated rather than discovered:** a secret passed as process *input* is no
longer protected anywhere, including stdout. The answer if that ever matters is to put it in
config, which is what config is for.

This reverses a shipped guarantee, so the tests that pin it are the record of the reversal and
must be updated deliberately rather than deleted:

- `secret_redaction_test.ts` (a config secret redacted from the API context) **inverts** — the
  value is returned.
- `secret_log_test.ts` (a config secret redacted in the *stored* trail) becomes a test that it
  is stored verbatim and redacted on **stdout**, which is a different assertion needing a
  different observation point: the server's log stream, not the logs endpoint. That is the one
  test that still pins a real guarantee, so it is the one that must keep biting.
- `secret_error_data_test.ts` (a `secret: true` inside a declared raise payload or fetch error
  body) **goes with the feature** — those markers are refused at registration now. It is
  replaced by a registration test that says so, which is the honest successor: the behaviour it
  guarded no longer exists to be tested. Extra protection on the read path (auth, a scoped token) is additive later and does
not change this shape.

## Collection: a grace window, because a reference is read separately

Splitting the read (`GET /instances/{id}` hands out refs; `GET /objects/{hash}` fetches them)
creates a race that `?resolve=true` did not have, because materializing was atomic with the
read: between the two calls the instance can move on, drop its claim, and take the object with
it. The client then 404s on a reference the server handed it moments earlier.

So collection is deferred, and the contract is stated rather than raced:

> **A reference you have been handed is fetchable for `--object-grace` (default 1h), whatever
> happens to the data that produced it.**

Mechanically, releasing a claim does not delete content — it leaves a **grace claim**:
`(hash, 'grace', '', expires_at = now + --object-grace)`. That is a ref like any other, so the
GC rule is unchanged and no new predicate appears anywhere:

- **On dereference**: drop the owner's ref, upsert the grace ref. No "was that the last one"
  check is needed — if another owner still holds the object the grace claim is simply redundant
  and lapses unnoticed.
- **In the sweep**, which must not be gated on anything else. Objects are released by ordinary
  work, so a deployment with log retention disabled still needs the sweep to run — the two share
  a tick and nothing else. [built: the first cut nested it inside `pruneLogs`, whose early return
  on `Retention <= 0` silently made every released object permanent.]
- **In the sweep**: drop refs whose horizon has passed, then delete content nothing claims. An
  object released longer ago than the window now has neither its owner's claim nor a live grace
  claim, and goes. Retiring expired refs first is what keeps the second statement a plain
  existence check.

The sweep never *stamps* a grace claim, only owners do. That is what keeps it from looping: a
grace claim that expires is dropped and the object collected on the same pass, rather than
earning another window forever.

**This retires 018's immediate deletion**, which existed so "a replaced value — and any secret
in it — does not linger". That property is deliberately given up here: §Redaction already moved
secret protection to recording and, ultimately, encryption at rest, so buying an hour's
reduction in at-rest exposure at the cost of a race every client must handle is the wrong trade.

### An object in its window is unclaimed, not dead — and resurrection races the sweep

Content is addressed by hash, so writing the same bytes again finds the row that is already
there. An object sitting on nothing but a grace claim is therefore **not** waiting to die: the
next write of that content claims it, and the object is live again with no copy made. That is
the ordinary case for a task that loops over two alternating values, and it needs no mechanism —
`PutObject` conflicts, `PutObjectRef` claims, done. The stale grace claim is left alone; it
expires harmlessly while a live claim exists, and a later release upserts it with a fresh
horizon.

**But the naive upsert has a race with the sweep, and it is the DO NOTHING that causes it.**
Interleave a writer resurrecting content with the collector:

1. writer: `INSERT … ON CONFLICT (hash) DO NOTHING` — the row exists, so **nothing is written
   and no lock is taken**
2. sweep: `DELETE FROM objects WHERE NOT EXISTS (a ref)` — the writer's ref is not committed
   yet, so the object is deleted
3. writer: `INSERT INTO object_refs …` — a claim on content that no longer exists

The result is a dangling ref and a value silently lost. SQLite's single writer hides it; on
Postgres under READ COMMITTED it is reachable. The fix is to make the upsert an actual write so
it takes the row lock:

```sql
INSERT INTO objects (hash, content, size, created_at) VALUES (…)
ON CONFLICT (hash) DO UPDATE SET size = excluded.size;   -- NOT "DO NOTHING"
```

`size` is the same value by construction — one hash, one content, one length — so the update
changes nothing and exists only to hold the row against a concurrent delete, which then
re-evaluates its predicate and finds the new claim. **`DO NOTHING` is the reading that looks
obviously right here**, which is why the reason belongs in the query.

One property falls out and is worth keeping: because a writer always supplies the bytes, an
object deleted while a ref survives is *re-created* by the next write of that content. That
makes the store self-healing against this class of bug rather than merely broken, and it is the
reason the race is a silent loss for readers in between rather than permanent corruption.

### The window is the dominant storage cost for churny processes

A grace window converts "the store holds live values" into "the store holds live values **plus
everything released in the window**". For a looping task that rewrites a 10 KB output every
second, an hour is ~3,600 objects and ~36 MB that no one can reach except by a reference handed
out before the release — at 24 hours it would have been ~86,000 and ~860 MB.

That is a real price for a race a client hits only if it reads, pauses, and fetches. So the
window is a flag: **`--object-grace`, default 1h** [decided 2026-08-24], alongside
`--log-retention` and read the same way. An hour is orders of magnitude more than a
read-then-fetch needs (client latency is seconds) and short enough that churn does not dominate;
a deployment handing references to slow consumers raises it.

The guarantee has to be *stated* rather than implied — a client that holds a reference longer
than the window and expects it to work is the failure this section exists to make legible.

**One narrower race is deliberately not covered.** An object whose only claim was a *log* ref
that reached its retention horizon is collected by the sweep with no grace window — the ref
expired rather than being released. Covering it would mean the sweep stamping grace claims,
which is the looping case above. The window is "this object's retention lapsed between your read
and your fetch", on a horizon measured in days rather than the instant a task overwrites its
output, so it is a coincidence rather than an ordinary interleaving.

### Sharing does not weaken it — a note, because it looks like it does

Two instances holding byte-identical content are one object, so when one replaces its value the
content stays. That reads like a regression against 018's "a replaced value — and any secret in
it — does not linger", and it is not. It was written down here as a trade to accept, and that
framing was wrong.

The property 018 wanted is **no object outlives every claim on it**, and refs preserve it
exactly. The surviving bytes are the other instance's *live data*, correctly retained: there is
no reading under which one value is at once A's-and-should-be-gone and B's-and-should-stay.
Nothing crosses between them either — sharing happens only when B independently produced
identical bytes, so B gains no access it did not already have, and a read is addressed by a hash
that already implies the content.

What genuinely changes is smaller and is not about secrets: the retention window for a given
byte-sequence becomes the maximum over its holders rather than per-holder. Purging one
instance's data has never removed another instance's identical data, and does not start to now.

### A definition claim is permanent, and needs no special case [decided 2026-08-24]

A `definition` ref carries no horizon and is never dropped: nothing deletes a definition
version. That is fact rather than policy — there is no delete endpoint and no
`DELETE FROM process_definitions` anywhere — and it is the retention rule code needs anyway,
since an instance pinned to an old version must still be able to load its bundle.

**The general rule already implements it.** "Collectable when no ref remains" means an object
with a definition claim is never collectable; the GC does not need to know what kind of claim it
found, and no branch is added for one. That is the test of whether the ref model was the right
shape, and it passes.

Two consequences worth stating so neither is a surprise:

- **Definition objects accumulate monotonically**, and the bound is *distinct bundle content*
  rather than versions or instances. Content dedupes globally, so a new definition version that
  changed its YAML but not its script claims the object that is already there. The cost is one
  object per distinct bundle ever applied, which is bounded by deploys that actually changed the
  code.
- **It does not foreclose deletion.** If a version ever becomes deletable, dropping its refs
  makes its objects collectable through the same rule as everything else — nothing about
  permanence is baked into the schema, only into the fact that nothing drops those refs today.

## Signatures and naming, settled

- **`ResolveObject` loses its owner parameter.** It takes an instance id today and, once reads
  are addressed by content, would not use it. A signature that accepts an owner it ignores is a
  lie the next reader has to disprove.
- **`owner_id` is a namespace shared by kind.** An `instance` ref and a `log` ref both carry the
  *instance* id — they are the same subject making two different claims — and a `definition` ref
  carries `name@version`. The pair `(owner_kind, owner_id)` is the owner; neither half alone is.
- **`GetLogObject` disappears** rather than being renamed: its whole body was the serving rule,
  and the serving rule is gone. Log payload reads become the same `GET /objects/{hash}` as
  everything else.

## The invariants the stress tests already encode

`tests/stress/gc_chaos_test.ts` and `object_deref_test.ts` are the real guardians of this
store — they assert reachability through crash, error, pause and retry chaos — and they read
`pinned` / `log_until` directly. They must be ported, not deleted, and the translation is exact:

| today | after |
|---|---|
| a row with `pinned = 1` | an `instance` ref whose `owner_id` is that instance |
| `log_until IS NOT NULL` | a `log` ref for that instance |
| alive iff `pinned OR log_until` | alive iff **any** ref exists |
| an unpinned, unlogged row is a leak | an object with **zero** refs is a leak |

`object_deref_test.ts` is the one that does not merely port: its subject — "a dereferenced,
unlogged context object is deleted **immediately** (not left for the sweep)" — is the behaviour
§Collection deliberately reverses. It becomes the opposite assertion, that a released object is
*retained* under a grace claim and reachable by a reference handed out before the release, and
its companion check that the store "does not accumulate" has to be re-scoped to the window
rather than to the live set. Rewriting a test to assert the opposite of what it was written for
needs the reason recorded next to it, which is what this paragraph is for.

The ported form is *stronger*: today a row proves only that someone pinned it, where a ref names
**who**, so "this instance's live context reference resolves to a claim held by this instance"
becomes checkable and is not today.

One tolerance carries over unchanged and should not be tightened: a `log` claim whose log row
was lost to a SIGKILL is horizon-alive and reclaimed at expiry, not a leak. The chaos test
deliberately checks the claim rather than a surviving log row, and that is why.

## The wire: an objects section, not markers in the data

`model.Envelope` exists, in its own words, so "user data is always nested under Data and is
never confused with the envelope itself — there is no in-band sentinel to collide with
arbitrary user JSON". The **wire does exactly what the disk format refuses**: an externalized
slot is returned as `{"ref": "9f2a…", "size": 221110}` sitting inside the context, which is
indistinguishable from a process whose output legitimately contains those two keys. The storage
layer solved this and the API layer reintroduced it one level up.

`?resolve=true` is the other half of the same problem. It materializes every externalized slot
into one response with no cap — a detail read on an instance holding 8 MB of outputs returns
8 MB — and it is a second server-side path for one concept.

Both go. Every response carrying value-slots gains one standardized section, and the data holds
only real values:

```jsonc
{
  "id": "…", "status": "completed",
  "context": { "outputs": { "price": { "fee": 25 } } },   // no markers anywhere
  "objects": [
    { "path": ["context", "outputs", "render"], "ref": "9f2a…", "size": 221110 }
  ]
}
```

- **`path` is an ARRAY of keys** rooted at the response body, not a JSON Pointer string.
  Rejected the pointer despite it being the standard: RFC 6901 escapes `/` as `~1` and `~` as
  `~0`, so every recipient — genctl in Go, a worker in TypeScript, anyone's client — has to
  implement the same unescaping before it can walk anywhere, and two implementations of one
  escaping rule is the failure this repo keeps refusing elsewhere. An array needs none: reading
  it is `path.reduce((o, k) => o[k], root)`.

  It is also *more* precise than a pointer, which is the part that settles it. JSON Pointer
  spells an array index as a decimal string, so `"0"` is ambiguous between the object key `"0"`
  and element zero; an array path can carry a JSON number for an index and a string for a key,
  and the ambiguity does not arise. `ObjectRef.Path` is reserved for exactly this and has been
  since 018.
- **No `url`.** There is one endpoint and the address is the ref, so a URL field would be a
  second thing that can disagree with the first.
- **The slot is ABSENT from the data**, not null and not a marker. That is the one tradeoff:
  absent is ambiguous with "the task produced nothing", where a marker was not. It is the
  better failure — a client that ignores the section sees a *missing* value rather than a
  plausible object it will treat as data — and a client that reads the section, which is the
  contract, sees no ambiguity at all.

`resolve=true` returns in a bounded form (§Resolution is automatic while it is small); what goes
for good is `HydrateContext`'s unbounded materialization and the truncated preview that existed
only to make an unresolvable payload legible. A log entry lists
its own externalized payload, the same way an instance detail lists its context slots; §A section
belongs to whatever object owns its values is where the paths are rooted and why, and §A log
payload is a value is why there is nothing else to it.

### A section belongs to whatever object owns its values [decided 2026-08-24]

The paths are rooted at the object carrying the `objects` field — not at the response. On an
instance detail those coincide: the body owns the context, so a path reads
`["context", "outputs", "x"]`. On a **list they do not**, and each *entry* carries its own
section with paths rooted at the entry: `["data"]`, never `["items", 3, "data"]`.

The distinction is not "logs are special", it is that a path made of **names** is stable under
anything a client does, while a path containing a **position** is valid for exactly one
unmodified page. `genctl` accumulates pages into one slice and reverses rows before rendering
([http.go](../cmd/genctl/http.go) `listHead` / `fetchOrdered`), so page two's `items[3]` is
`all[53]` before anything reads it. Rooting the section at the entry removes the question: the
section travels with its owner, and paging, sorting and streaming are none of its business.

**[built] The first cut got this wrong twice** — once with response-rooted paths into a list
(broken by the accumulate-and-reverse above), then by giving log entries a bare `data_ref`
instead. The second was defensible on ambiguity grounds and still wrong on shape: one concept
should have one spelling, and `objects` is it. A recipient now implements the protocol once,
against "this object lists its own values", and applies it wherever it finds the field.

### A log payload is a value, cut like any other [decided 2026-08-24]

A log entry's `data` is the **value** — not a rendering of it. The engine carries `any` from the
event to storage, cuts it with the same `cutForSize` a context slot gets, and renders text once,
for the console line, where a human is the reader.

It was a pre-rendered string, externalized whole when it exceeded the cap. Two consequences, both
observed: a string has no tree, so the cut had nothing to choose between and the whole payload
moved as one blob; and because each instance's payload differed in its *small* fields, no two
blobs ever hashed the same. Three runs of one 226 KB script cost **855,418 bytes** — one shared
context object plus three near-identical copies of the script under `log` claims. The same three
runs now cost **231,376 bytes**: one object, six claims (three `instance`, three `log`).

That is not a log optimization. It follows from the log having no shape of its own: the same cut
produces the same leaf, the same leaf hashes the same, and sharing is what content addressing
already does. The log-specific spellings die with it — `WriteLogObject`, `Envelope.Preview`, and
the entry-level "data absent, one ref listed" convention. An entry now reads like every other
response: the shell inline, `objects` naming `["data", "code"]`, and `genctl` splicing the
`{ref,size}` handle back into the place it was cut from.

### A ref is never stored in the data either [decided 2026-08-24]

The rule is not a wire rule. **Nowhere** — on the wire or on disk — does a reference sit inside
the value it stands for; it goes in a sibling `objects` list, with a path saying where it
belongs. The stored form is the same shape as the response:

```jsonc
// external_data, for a task whose code is a definition-owned object
{ "task_id": "price",
  "input":   { "input": { "amount": 250 } },              // the ref'd leaf is absent
  "objects": [ { "path": ["input", "code"], "ref": "9f2a", "size": 221110 } ] }
```

Storing it inline instead does not merely look inconsistent — it does not survive. A Go
`*ObjectRef` marshals to `{"ref":…,"size":…}` and comes back a plain map, so the type that says
"this is a reference" is gone, and the only way to recover it is to guess from the shape — which
misreads a task input that legitimately contains `ref` and `size` keys. Out of band, the list
*says* which paths are references and nothing has to be inferred.

It also removes work rather than adding it: the queue endpoint has the section already in the
shape the response wants, rooted the same way, so it forwards rather than re-deriving. And the
extract/place pair belongs in one shared place — it is currently written twice (`extractObjects`
in the API, `spliceObjects`/`place` in genctl) and the storage layer needs the same pair, which
is two copies too many.

`ObjectRef` gains `Path` — the field migration 018 reserved for exactly this and never built.

### Resolution is automatic while it is small [decided 2026-08-24]

Two consumers cannot follow a reference, and both get the same rule: **materialize what fits, and
make the consumer fetch the rest.**

- **A fetch body always resolves.** Its reader is a remote server that cannot call genroc, so a
  ref reaching it is a script that never arrives. The engine loads the object into the request
  before sending, and refuses past a cap rather than materializing something enormous into a
  request — a `pre.error`, since nothing left.
- **`?resolve=true` returns**, per object rather than per response: an object under the cap is
  spliced into the data, one over it stays listed for the caller to fetch. That answers the
  objection it was removed for — an *unbounded* response behind one query parameter — without
  costing the ergonomics. It degrades rather than failing: the answer is always usable, and a
  caller that ignores the section still sees a missing value rather than a wrong one.

The caps are safety limits, not tuning knobs, in the sense migration 018's 8 MiB response cap
already established: past them the right fix is to change what the definition sends, not to
raise the number.

### What the CLI does instead, and where it declines to help

The rule is "resolve where a human would want the value, print the handle where they would not",
because the two cases differ by size and by what is being read:

| | |
|---|---|
| `genctl get --resolve` | **resolves client-side.** Fetch each listed object, splice it in at its path. Context values are what the reader came for. |
| `genctl logs` | **does not resolve, ever.** A trail is scanned, not read; resolving turns one screen into megabytes, and the payloads are exactly the values large enough to have been externalized. It prints the object's id in place of the value. |
| `genctl object <ref>` | **new.** Fetch one object by id and print it. The escape hatch for the line you actually care about, and the whole reason printing an id is enough. |

`logs --resolve` therefore goes rather than moving client-side. That is not a gap: it is the
flag being wrong for its subject, which only became visible once resolving was the client's job
and the cost landed where it could be seen.

The splice itself is one implementation of "attach", in the client, exercised by `get --resolve`
on every run — and it is the same code a worker needs.

## What it buys, in order

1. **Cross-instance dedup**, immediately, for every externalized value — not just code.
2. **A place to own definition-embedded values.** A large Shape literal externalizes at
   `PUT /definitions` under a `definition` ref, evaluation passes the `ObjectRef` leaf through
   unchanged (`encodeContextValue` already does exactly this), and `external_data` holds
   `{code: {ref, size}}` instead of 221 KB.
3. **A cacheable handle for workers.** A sha256 ref is immutable by construction, so a worker
   fetches once and caches forever with no invalidation problem. The claim response shrinks to
   the ref.
4. **An answerable question**: who holds this object, and until when.

## Choosing what to externalize: a size-driven cut

Status: **BUILT 2026-08-24** (`internal/db/objectcut.go`), for the task-input slot. Context slots
still use the whole-slot rule; moving them onto the same cut is the remaining half.

### What is wrong with both current rules

There are two, and neither is about the thing that matters — how big the row ends up:

- **Whole-slot, over 2 KiB** (`encodeContextValue`, for context slots). All or nothing: a 2.1 KiB
  slot goes out entirely, and a slot's small fields travel to the object store with its big one.
- **Per-leaf, over 2 KiB** (`externalizeLeaves`, for a task input). Independent of the total, so a
  value of a hundred 1 KiB leaves — 100 KB — stays fully inline because no single leaf crosses
  the line, while a value of two 3 KiB leaves externalizes both even though removing one would
  have been enough.

Both ask "is this piece big" when the question is "is the row still too big".

### The algorithm

Externalize the **fewest, largest leaves** that bring the stored size under a target.

1. Encode once and record every node's encoded size, bottom-up.
2. `total = dataSize + objectsListSize`. If `total <= target`, externalize nothing.
3. Otherwise take the largest remaining **leaf**, move it to the object store, and account for
   it exactly: `data -= leafSize`, `objects += entrySize(path)` — the value is removed rather
   than replaced, so the delta needs no re-encoding of anything else.
4. Repeat until `total <= target`, or until no candidate is worth taking (below).

Leaves first, and that is not an aesthetic preference: it is what preserves **sharing**. The
motivating value is a task input holding a bundle beside per-instance data. Cutting the leaf
isolates the bundle, so every instance of a definition version produces the same hash and stores
it once. Cutting the parent folds the per-instance data in, giving every instance a different
hash and no sharing at all — the whole win, lost to a coarser cut.

### Going up a level

If every leaf is taken and `total` is still over target, the skeleton itself is too big: a
thousand refs is a thousand entries in the objects list. Then the cut **coarsens** — candidates
become the parents, and choosing a parent **removes its descendants from the cut**.

That removal is the load-bearing part. An object's content is opaque bytes; nothing walks inside
it looking for references, so a ref nested in an object's content would never be resolved. The
cut must therefore be an **antichain**: no chosen node is an ancestor or descendant of another.
Coarsening means the parent is stored whole, with what would have been its children's objects
**inlined back into it** — which at write time costs nothing, because the values are still in
hand and were never separated.

Coarsening terminates: the root as a single object leaves a slot holding one reference, which
always fits.

### Two rules that fall out

- **A floor.** Removing a leaf costs an objects entry (path, hash, size — call it ~80 bytes). A
  leaf smaller than its own entry makes the row *bigger*. So a candidate under the floor is not
  taken, and when the largest remaining leaf is under it, that is the signal to coarsen rather
  than to keep going.
- **Determinism, which dedup depends on.** Two instances must choose the *same* cut, or they
  produce different objects for identical content and share nothing. Ties therefore break on a
  total order — size descending, then path ascending — and never on Go's map iteration, which is
  randomized. This is the detail most likely to be dropped and least likely to be noticed: it
  degrades sharing quietly rather than failing.

  **[built]** There are two independent defences, not one: map children are built in sorted key
  order, *and* equal candidates break their tie on path. Removing either alone leaves the result
  deterministic — only removing both makes it vary, which is what the test had to be checked
  against. Keep both: a single defence with no second is one edit away from silent unsharing.

### Cost

One encode to size the tree, then arithmetic — no re-encoding per round, because removing a
value changes only its own bytes and leaves every key and separator around it untouched. The
selection is a sort plus a walk.

## Phasing

1. **The store.** ✅ **Built 2026-08-24.** Migration 029, `objects` + `object_refs`, the context
   and log paths on claims, `--object-grace`, `GetLogObject` gone, `ResolveObject` without its
   owner. Both stress tests ported — `gc_chaos` now reports `shared=25` of 32 objects under
   crash chaos, which is the cross-instance dedup this exists for, observed rather than argued.
2. **The wire.** ✅ **Built 2026-08-24.** The `objects` section, `GET /objects/{hash}`,
   `HydrateContext` deleted, `resolve=true` bounded, genctl splicing client-side plus
   `genctl object`. Log entries are listed like any other value -- literally: §A log payload is
   a value, cut like any other.
3. **Definition objects.** ✅ **Built 2026-08-24**, and not the way this doc proposed. It said
   externalize large Shape *literals* at apply time; what shipped externalizes a task input
   **leaf by leaf when the task is stored**, which needs no apply-time pass, no refs inside a
   definition, and no version to own them. Content addressing does the work the apply-time step
   was for: every instance of a version evaluates the same bundle to the same bytes, so the
   second write finds the object already there. The cost is hashing it per instance (~1 ms),
   against a design that would have had to make a `*ObjectRef` survive definition storage — the
   same round-trip problem, one layer up.
   Leaf by leaf and not whole: externalizing the entire input would fold the per-instance data
   in with the bundle, giving every instance a different hash and no sharing at all.
4. **Worker caching.** ✅ **Built 2026-08-24.** `evaluator/worker.ts` follows the entry's objects
   section and caches by content hash, which cannot invalidate.

**Measured, on the fixture §The measurement opened with** — a 221 KB script, ten instances:

| | before | after |
|---|---|---|
| ten instances' `external_data` | 2,233,580 B | **1,360 B** |
| objects stored | 0 | **1**, claimed ten times |
| claiming three tasks | 670,686 B | **1,020 B** |

**The wire comes before definition objects, which is not the order this doc first proposed.**
Definition objects put a `{ref}` into an external task's `input`, so a worker claiming that task
would receive an in-band `{ref, size}` marker — exactly the shape §The wire exists to delete, and
its tests would encode it. Building the carrier before the thing it carries is the only order
that does not ship a shape twice.

## Open

- **Whether an `instance` ref is per-slot or per-instance.** Per-instance (today's shape,
  one ref per (instance, hash)) keeps `applyContextObjectDiff`'s whole-context diff. Per-slot
  would let a write know less than the whole picture, which is the point of §3 above — but it
  multiplies rows and the diff is not currently a problem. Start per-instance.
- **Batch fetch.** N refs in a response is N round trips. The common case is one (a task's
  code), so this is an optimization rather than a requirement — but a detail view of an
  instance with ten externalized outputs is ten calls, and `genctl get --resolve` is the thing
  that will feel it first.
- **Encryption at rest**, which is what actually protects an object's content and is the reason
  redaction-on-read was dropped rather than reimplemented. Out of scope here; the shape above
  does not foreclose it (content is opaque to every layer but the one that wrote it).
