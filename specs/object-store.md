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

(b) was the hard one to move, because it runs at the call site inside the audit snippet
(`Engine.snippet`), collapsing
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
changes nothing and exists only to hold the row against a concurrent delete. **`DO NOTHING` is
the reading that looks obviously right here**, which is why the reason belongs in the query.

**[corrected 2026-08-24] The lock is half the fix, and this doc claimed it was the whole one.**
It said the blocked DELETE "then re-evaluates its predicate and finds the new claim". It does
not. Postgres wakes a blocked DELETE and re-checks the newer version of the TARGET ROW; the
`NOT EXISTS` subquery keeps the statement's original snapshot, so it still reports no claims and
the delete proceeds. Measured, not argued: the writer commits its claim, the sweep unblocks, and
the result is `objects = 0, claims = 1` — the exact dangling ref the upsert was supposed to
prevent, still there with `DO UPDATE` in place.

The other half is on the sweep. Read committed takes a fresh snapshot per **statement**, so the
wait and the decision must be two statements:

```sql
SELECT hash FROM objects o WHERE NOT EXISTS (…) FOR UPDATE;  -- blocks on the writer
DELETE FROM objects o WHERE NOT EXISTS (…);                  -- fresh snapshot: sees the claim
```

`collectUnreferencedPG` does this in one transaction; SQLite keeps the single statement, because
its single writer means no claim can commit between a statement's snapshot and its delete. Both
defences are now pinned by `TestObjects_ContentSurvivesASweepRacingItsResurrection`, which fails
with a distinct message when either is removed — neither works alone.

Worth recording for the next invariant of this kind: the stress test that was supposed to cover
this (`TestObjects_ResurrectionAgainstALiveSweeper`) stayed green through eight runs with BOTH
defences dismantled. The window is the microseconds between two adjacent statements inside one
transaction; chance will not find it, and a test that hopes to is a test that reports success.

**Both defences assume one transaction, and the log path did not have one.** The row lock lasts
as long as the statement that took it, so two autocommit statements -- write the content, then
claim it -- leave the object committed and held by nobody in between. `CutLogValue` wrote that
way, and the gap was not microsecond-narrow: an observer polling for "an object nobody claims"
caught it **452 times across 200 log writes**, more than twice per write. SQLite is not spared
either; its single writer stops concurrent *writes*, not a sweep running between two committed
ones. The fix is the same transaction the context path already had (`db.withTx`), and the count
goes to zero -- which is what `TestLogObjects_ContentAndClaimAreWrittenAtomically` asserts.

The general rule this leaves: **an object writer that is not transactional has no defences at
all**, however carefully the upsert and the sweep are written.

So the addition half is now one function, `claimObjects`, taking a transaction's queries — the
instance path joins the instance write, the log path opens its own. Owner and horizon are the only
parameters that vary. `archtest.TestObjectWritesGoThroughClaimObjects` fails on any other caller of
`PutObject`/`PutObjectRef`, because both times this broke it was a second copy of the same loop.

**Removal stays separate on purpose.** An instance claim is dropped when the value stops
referencing it and leaves a grace stamp; a log claim is never dropped — it carries a horizon and
the sweep retires it. One is "does this value still point here", the other "has this claim's time
passed". Merging them would mean inventing a release logs do not have, or giving an instance claim
an expiry, which is a silent way to delete live data.

**But the guarantee must be the same, and the reason it was not is one wrong join**
[fixed 2026-08-24]. `object_refs` is an n:n table between objects and the things that hold them,
and `owner_id` should always BE the holder. For `instance` it is the instance and for `definition`
the version — but for `log` it was the **instance**, while the thing that actually carries the
reference is the log **row**.

Everything awkward followed from that. A row's deletion said nothing about whether the claim was
still wanted, so a log claim could not be released the way an instance value is; so its life was
guessed with a retention horizon; so the grace window had to be folded into the horizon; so object
lifetime was coupled to `--log-retention`, with a `logForeverMillis` sentinel for "retention
disabled". Three mechanisms compensating for a join to the wrong entity.

`owner_id` is now the log row's id (both columns are TEXT, so no schema change), and all of it
collapses to the rule everything else already followed: **an owner releases, and the release stamps
grace.** The sweep notices a claim whose owner row is gone — `NOT EXISTS (SELECT 1 FROM
process_logs …)` — releases it and stamps grace. Driven by the owner being absent rather than by
ids the prune collected, so a crash between deleting rows and releasing claims is repaired by the
next sweep instead of leaking. `SetObjectRetention`, `logForeverMillis` and the horizon arithmetic
are deleted rather than replaced.

Two things the change forced, both worth keeping:

- **A log row with objects is written synchronously, row and claims in one transaction**
  (`AppendLogValue`). Log rows are normally buffered and flushed in batches; a buffered row would
  leave a claim whose owner does not exist yet, and the orphan sweep retires exactly those. Rows
  with no objects — nearly all of them — keep the batched path.
- **The sweep may stamp grace here and nowhere else.** The old rule was "only owners stamp grace,
  never the sweep", protecting against a grace claim earning itself another window forever. The
  real requirement is termination: a *grace* claim must never be re-graced, while a log claim
  retired once has no owner left to retire it again.

Migration: none. Claims written under the old ownership carry a horizon and an instance id, and
`OrphanedLogRefs` is scoped to `expires_at IS NULL` so they drain on their horizon exactly as
before rather than all looking orphaned at once. The clause is vacuous afterwards.

### The grace window is a mark, not a claim [decided 2026-08-24]

A grace claim was a row in an ownership table whose `owner_id` was `''` — not an owner, a timer
wearing a claim's costume. What exposed it was asking who is responsible for stamping one.

**No releaser can be.** An owner dropping its claim cannot tell whether it dropped the LAST one;
it knows only its own references. So "stamp a grace claim on release" was a distributed obligation
nobody was in a position to satisfy, and it showed: the rule "only owners stamp grace, never the
sweep" needed an exception the first time a new releaser appeared (orphaned log claims), and any
claim retired by the expiry sweep got no window at all.

The sweep is the one component that sees every claim, so it decides. `objects.released_at` is the
mark, and the sweep maintains it in three steps:

```sql
UPDATE objects SET released_at = NULL  WHERE released_at IS NOT NULL AND EXISTS (a claim);
UPDATE objects SET released_at = $now  WHERE released_at IS NULL     AND NOT EXISTS (a claim);
DELETE FROM objects WHERE NOT EXISTS (a claim) AND released_at < $now - $grace;
```

Every owner is then free to care only about its own references — add them, remove them, done.
Gone with it: `owner_kind = 'grace'`, `GraceOwnerID`, `expires_at` on `object_refs`,
`DropExpiredObjectRefs`, and the never-re-stamp invariant that made the whole thing terminate.

**The mark is cleared in two places, and each covers what the other cannot.** The sweep's own pass
handles a claim added without re-writing content — what a passed-through reference does. But a
claim made and released entirely BETWEEN two sweeps is invisible to it: by the time the next sweep
looks, the object is unclaimed again and carrying a mark from before a claim nobody observed,
already older than the window. `PutObject`'s conflict path clears it at the instant the claim is
made, and that statement is already writing the row to take the sweep's lock — so the second
defence is free. `size` is written for the lock; `released_at` is written because it is true.

**The cost is stated rather than hidden.** The window starts when the sweep notices, not when the
release happened, and the sweep is a once-a-minute janitor — so released content lingers up to a
minute past `--object-grace`. That errs toward keeping data, and it is why an object can be
legitimately unclaimed AND unmarked, which is a state the old model could not produce. The stress
tests assert "nothing overdue" instead of "everything claimed" for exactly that reason.

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

### Every owner declares its references in an `objects` field [built 2026-08-24]

The rule below says a reference never sits inside the value it stands for. On disk it used to sit
one step away instead — in a `model.Envelope {data, refs}` wrapper per column — which is beside
the value but still *per slot*, so "what does this owner reference" was a question you answered by
knowing the storage layout of every column.

Something did need to ask it. `gc_chaos_test.ts` reconstructs an owner's references out of
`input_data`, `output_data`, `outputs_data.items` and `process_logs.data` before it can compare
them against `object_refs` — a second copy of the encoder's layout, free to drift from it. Now
each owner carries one `objects` column and the comparison is a read.

It also lines storage up with two things it already had to agree with: the **wire**, where the API
lists one `objects` section per owner with `{path, ref, size}`, and the **claims**, which are
keyed per owner rather than per slot. `model.Envelope` is deleted — the concept exists nowhere on
disk any more.

**The cost is real and landed immediately.** A slot's value and its references are no longer one
value, so a write that carries the columns can drop the declaration without a compile error. Three
call sites missed it within minutes (`RetryProcess` passing raw columns through, the parent park in
`SpawnChildrenAndWait`, and one more the guard found), and the damage is invisible to the GC —
claims are what it reads — until something compares the two. `archtest.TestInstanceWritesCarryObjects`
is that price paid once.

Definitions get no such column: `ObjectOwnerDefinition` is declared and nothing claims under it, so
there is nothing to declare. Adding one would be building for a feature that does not exist.

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
   unchanged (the slot cut already does exactly this), and `external_data` holds
   `{code: {ref, size}}` instead of 221 KB.
3. **A cacheable handle for workers.** A sha256 ref is immutable by construction, so a worker
   fetches once and caches forever with no invalidation problem. The claim response shrinks to
   the ref.
4. **An answerable question**: who holds this object, and until when.

## Choosing what to externalize: a size-driven cut

Status: **BUILT 2026-08-24** (`internal/db/objectcut.go`) for the task-input slot; **context
slots followed**, so `cutSlot` now runs the same `cutForSize` against `contextObjectThreshold`
and the two rules below are both gone.

### What was wrong with the two rules this replaced

Neither was about the thing that matters — how big the row ends up:

- **Whole-slot, over 2 KiB** (`encodeContextValue`, for context slots; removed). All or nothing:
  a 2.1 KiB slot went out entirely, and a slot's small fields travelled to the object store with
  its big one.
- **Per-leaf, over 2 KiB** (`externalizeLeaves`, for a task input; removed). Independent of the
  total, so a value of a hundred 1 KiB leaves — 100 KB — stayed fully inline because no single
  leaf crossed the line, while a value of two 3 KiB leaves externalized both even though removing
  one would have been enough.

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
4. **Worker caching.** ✅ **Built 2026-08-24.** `eval-node/worker.ts` follows the entry's objects
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
