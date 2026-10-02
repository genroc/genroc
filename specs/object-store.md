# The object store: content, and who holds it

Status: Built.

A large value is stored once, globally, addressed by its content, and held through explicit
claims. The trigger was script tasks: a 221 KB bundle was copied into every instance's
`external_input` (ten instances: 2,233,580 B) and re-shipped on every claim. Now ten instances
store 1,360 B between them and one object claimed ten times.

## The shape

    objects(hash PK, content, size, created_at, released_at)
    object_refs(hash, owner_kind, owner_id, created_at, PRIMARY KEY (hash, owner_kind, owner_id))

`hash` is the first 128 bits of the content's sha256, hex: identity and change-detection key at
once, so byte-identical content from any owner is one row. A claim names the entity that actually
carries the reference:

- `instance` — a context value; `owner_id` is the instance. Released when the value stops
  referencing the hash (`applyContextObjectDiff`, diffing against what the write loaded).
- `log` — a log payload; `owner_id` is the **log row**, so the claim is wanted exactly while the
  row exists. The sweep releases claims whose row is gone (`OrphanedLogRefs`), driven by the owner
  being absent rather than by ids a prune removed, so a crash between the two self-repairs. Keying
  it by instance instead forces a retention horizon onto the claim and couples object lifetime to
  `--log-retention`.

Releasing a claim deletes nothing; whether content is collectable is the sweep's question alone
(§The grace window is a mark).

## What this must not break

- **Deletion is "no claim remains", never "my claim went".** Content is shared, so the second
  rule destroys an unrelated instance's value while its own instance still resolves.
- **Reads are addressed by content, and that is the whole access rule.** `GET /objects/{ref}`
  consults no claim: knowing a hash is knowing the bytes. What it discloses is **existence** —
  that some owner holds exactly these bytes — which bites only for content an attacker can
  reconstruct byte-for-byte. `ResolveObject` therefore takes no owner; one it ignored would imply
  a check that does not exist.
- **`owner_kind` governs lifetime, not access.**

## Redaction is a recording concern, not a read concern

`secret: true` means "do not print this". Redaction happens in one place, the server's own stdout
(`audit()` scrubs the console copy); the stored trail and every API response carry values
verbatim. Protecting values at rest is encryption's job (§Open), not a second API shape.

A secret passed as process *input* is therefore protected nowhere, stdout included — put it in
config. `secret_log_test.ts` pins the one real guarantee (stored verbatim, redacted on stdout);
`secret_redaction_test.ts` pins that the API returns the value.

### `secret: true` is CONFIG-ONLY

Valid in `config_schema`, **refused at registration anywhere else** (`Schema.ContainsSecret`), so
a definition expecting protection is told it will not get it. The scrub is `redactSecrets`:
string replacement of the known values `def.SecretConfigValues(inst.Config)`, airtight because
expressions have no functions, so a secret always appears verbatim. It cannot scrub a value it
does not know — a fetch response body never enters the context — so a marker there would promise
a scrub nothing delivers. A structural redactor for bodies would have to stringify before
`audit()`, leaving nothing unredacted to store.

## Collection: a grace window, because a reference is read separately

A read hands out references; fetching them is a second call, between which the data can move on
and release its claim. The contract:

> **A reference you have been handed is fetchable for `--object-grace` (default 1h), whatever
> happens to the data that produced it.**

### An object in its window is unclaimed, not dead — and resurrection races the sweep

Writing the same bytes again claims the row already there, with no copy. That races the sweep,
and needs **two defences, neither sufficient alone**:

1. `PutObject` is `ON CONFLICT (hash) DO UPDATE SET size = excluded.size, released_at = NULL` —
   **not `DO NOTHING`**, which writes nothing, takes no row lock, and lets the sweep delete the
   object between the upsert and the claim, leaving a dangling claim. `size` is unchanged by
   construction; it is written to hold the row.
2. The Postgres sweep is `SELECT … FOR UPDATE` **then** `DELETE`, two statements in one
   transaction (`collectUnreferencedPG`). A single `DELETE` woken from a row lock re-checks only
   its target row; its `NOT EXISTS` keeps the statement's original snapshot and deletes
   just-claimed content. SQLite keeps one statement: its single writer lets nothing commit
   between snapshot and delete.

`TestObjects_ContentSurvivesASweepRacingItsResurrection` (Postgres) fails distinctly for each.
`TestObjects_ResurrectionAgainstALiveSweeper` stayed green with both removed: chance does not
find a window between two adjacent statements.

**Both assume the content write and its claim share a transaction** — the lock ends with the
statement that took it. So addition has one spelling, `claimObjects`, which takes a transaction's
queries: the instance path joins the instance write, `AppendLogValue` opens its own
(`archtest.TestObjectWritesGoThroughClaimObjects`, `TestLogObjects_ContentAndClaimAreWrittenAtomically`).
It claims every hash the value **references**, not only those it wrote (lazy-context.md §A
reference must not cross a boundary).

**Removal stays separate on purpose.** An instance claim is dropped when the value stops pointing
at it; a log claim when its row is gone. Merging them means inventing a release logs do not have,
or giving instance claims an expiry — a silent way to delete live data.

A log row with objects is written **synchronously**, row and claims in one transaction: a buffered
row would leave a claim with no owner yet, which the orphan pass retires. Rows without objects —
nearly all — stay batched.

### The grace window is a mark, not a claim

`CollectObjects` runs with the log prune (once a minute, `logPruneInterval`) but is **not gated on
log retention**: ordinary work releases objects, so `--log-retention 0` must still collect. In
order:

```sql
-- 0. release log claims whose row is gone (OrphanedLogRefs)
UPDATE objects SET released_at = NULL  WHERE released_at IS NOT NULL AND EXISTS (a claim);
UPDATE objects SET released_at = $now  WHERE released_at IS NULL     AND NOT EXISTS (a claim);
DELETE FROM objects WHERE NOT EXISTS (a claim) AND released_at < $now - $grace;
```

The sweep decides because **no releaser can**: an owner dropping its claim cannot tell whether it
was the last. (A `grace` claim stamped on release was exactly that unsatisfiable obligation.)

The mark is cleared in two places, each covering what the other cannot: the sweep's own pass
catches a claim added without re-writing content (a passed-through reference;
`TestObjects_ResurrectionClearsTheReleaseMark`), and `PutObject`'s conflict path catches a claim
made and released between two sweeps, whose mark would otherwise already be past the window
(`TestObjects_AClaimBetweenTwoSweepsStillEarnsAWindow`).

The window starts when the sweep *notices*, so released content lingers up to a minute past
`--object-grace`, and an object can be legitimately unclaimed and unmarked — the stress tests
assert "nothing overdue", not "everything claimed".

### The window is the dominant storage cost for churny processes

The store holds live values **plus everything released in the window**: a loop rewriting a 10 KB
output every second holds ~36 MB at 1h. Hence a flag, defaulting to an hour — far beyond a
read-then-fetch, short enough that churn does not dominate.

### Sharing does not weaken it — a note, because it looks like it does

When one holder replaces a shared value the content stays, because it is another holder's live
data: no object outlives every claim on it. Sharing happens only between owners that produced
identical bytes independently, so it grants no access.

## The invariants the stress tests encode

`tests/stress/gc_chaos_test.ts` and `object_deref_test.ts` guard the store through crash, error,
pause and retry chaos: an object with no claim and an overdue mark is a leak; a live context
reference resolves to a claim its instance holds; a released object stays fetchable inside its
window. One tolerance must not be tightened: a log claim whose row a SIGKILL lost is a pending
release for the next orphan pass, not a leak.

## The wire: an objects section, not markers in the data

Every response carrying values lists what it could not inline, and the data holds only real
values:

```jsonc
{
  "id": "…", "status": "completed",
  "state": { "outputs": { "price": { "fee": 25 } } },   // no markers anywhere
  "objects": [ { "path": ["state", "outputs", "render"], "ref": "9f2a…", "size": 221110 } ]
}
```

- **The slot is ABSENT**, not a `{ref, size}` marker, which would be indistinguishable from output
  that legitimately has those keys. A client ignoring the section sees a missing value rather
  than plausible data.
- **`path` is an array of keys**, not a JSON Pointer: no RFC 6901 unescaping in every client, and
  an index is a number where a key is a string, so `"0"` is never ambiguous.
- **No `url`** — the ref is the address; a second field could disagree with it.
- **A display puts the marker back.** genctl's `withObjectRefs` renders `{ref, size}` at the path
  for `get`, `detail` and `logs`; `--json` stays verbatim, section and all.

A ref is immutable, so `eval-node/worker.ts` caches objects by hash with no invalidation.

### A section belongs to whatever object owns its values

Paths are rooted at the object carrying the `objects` field: the instance body
(`["state", …]`, `["output"]`), each log entry (`["data", …]`), each claimed task
(`["external_input", …]`) — never `["items", 3, …]`. A path of names survives anything a client
does; a position is valid for one unmodified page, and genctl accumulates and reverses pages
before rendering.

### A log payload is a value, cut like any other

The engine carries `any` from the event to storage, cuts it with the same `cutForSize` a slot gets
(target `--log-payload-bytes`, default 2048), and renders text once, for the console. A
pre-rendered string has no tree, so it moved as one blob whose small per-instance fields defeated
sharing: three runs of a 226 KB script cost 855,418 B that way and 231,376 B now (one object, six
claims).

### Every owner declares its references in an `objects` field

`process_instances.objects` and `process_logs.objects` hold `[{path, ref, size}]`, one list per
owner rooted at its value — the same shape as the wire, and keyed like the claims, so comparing
an owner with the store is two reads. The cost: a write carrying value columns can drop the
declaration without a compile error, and the GC (which reads claims) never notices —
`archtest.TestInstanceWritesCarryObjects`.

### A ref is never stored in the data either

A Go `*ObjectRef` marshals to `{"ref", "size"}` and comes back a plain map; recovering it means
guessing from shape, which misreads user data with those keys. Out of band, the list says which
paths are references. API and storage share one traversal, `model.Extract` / `model.Place`.

### Resolution is automatic while it is small

Materialize what fits; make the consumer fetch the rest.

- **A fetch body always resolves**: its reader cannot call genroc. Past 8 MiB of referenced
  content the instance fails (`engine.expression`) rather than building an enormous request.
- **`?resolve=true`** on the instance detail splices each object up to 1 MiB and leaves larger
  ones listed, so the response stays bounded and degrades rather than fails.

The caps are safety limits, not tuning knobs: past them, change what the definition sends.

### What the CLI does instead, and where it declines to help

| | |
|---|---|
| `genctl get` / `detail --resolve` | splice client-side (`detail` asks the server's `?resolve=true` first) |
| `genctl logs` | **never resolves**: a trail is scanned, and its payloads are large by definition; it prints the handle |
| `genctl object <ref>` | fetches one object — the escape hatch that makes printing a handle enough |

## Choosing what to externalize: a size-driven cut

`cutForSize` (`internal/db/objectcut.go`) serves every value slot (target 2 KiB per slot, each task
output cut on its own) and log payloads.

### The algorithm

Externalize the **fewest, largest** pieces that bring `data + objects list` under the target.
Sizes are computed once, bottom-up. Candidates are taken deepest level first, largest first within
a level; only when a level is exhausted, or its largest remaining candidate is under the floor,
does the cut coarsen a level up — and choosing a parent un-chooses its descendants. Each round's
selection is spliced and measured, and re-selected if still over.

**Leaves first preserves sharing**: cutting the bundle alone makes every instance hash it the
same; cutting its parent folds per-instance data in and shares nothing.

**The cut is an antichain.** Object content is opaque — nothing resolves a ref inside it — so no
chosen node may contain another, nor an already-external marker (that marker would be baked into
the object and its claim released while still pointed at). The root as one object always fits, so
coarsening terminates.

### Two rules that fall out

- **A floor** (`minExternalizeBytes`, 128 B): a value smaller than its own objects entry makes the
  row bigger.
- **Determinism, which dedup depends on.** Two instances must choose the same cut or they share
  nothing, silently. Ties break on size descending then path ascending, *and* map children are
  built in sorted key order. Either alone suffices; keep both, or one edit makes sharing quietly
  degrade.

## Open

- **Per-slot `instance` refs.** Per-instance today; per-slot only if the whole-context diff
  (`applyContextObjectDiff`) becomes a problem.
- **Batch fetch.** N refs is N round trips; the common case is one (a task's code). Build when a
  view resolving many refs feels it — `genctl get --resolve` first.
- **Encryption at rest** — what actually protects content, and why redaction-on-read was dropped.
  Build when values at rest need protection; content is opaque to every layer but its writer, so
  nothing above forecloses it.
