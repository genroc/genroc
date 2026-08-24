# Lazy context access

**Wishes 1 and 3 BUILT 2026-08-24; wish 2 (path-level laziness in expressions) remains
proposal.** Follows [object-store.md](object-store.md), which put a
`Path` on every reference and made the store content-addressed. That work made a slot's
references *addressable*; this one makes them *invisible* — asked for `outputs.x.y`, the context
loads what that path needs and nothing else.

## The target

1. **One accessor.** A caller asks the context for a path. Whether `outputs.x` is inline, cut in
   three places, or a single whole-slot reference is the context's business, not the caller's.
2. **Laziness at the path, not the slot.** Reading `outputs.x.y` must not load the 200 KB leaf
   sitting at `outputs.x.code`.
3. **Untouched means unloaded.** A value an advance never reads must reach the next write as the
   reference it already was — no load, no re-hash, no new object.

## What already holds

- References carry `Path`, so "which object covers this path" is answerable from what is stored.
- `cutForSize` treats an `*ObjectRef` in the value as an already-externalized leaf and re-emits
  it with no new object ([objectcut.go](../internal/db/objectcut.go)). **Pass-through storage
  already works** — wish 3 is blocked by the read path, not the write path.
- `collectRoots` ([refs.go](../internal/expression/refs.go)) is a static reference analysis over
  the parsed expression, with the invariant that makes it safe: *over-report is waste,
  under-report serves nil.*
- `model.Extract` / `model.Place` is the one traversal, shared by storage and the API.

## What blocks each wish

1. Five on-disk shapes for one idea (`Envelope`, the `outputs` wrapper, `error.data`,
   `external_data`'s sibling `objects` key, `engine_state`), and `loaded` collected by hand at
   five sites in `decodeContext`.
2. `Roots` is name-level (`Outputs []string`), and `buildEnv` resolves whole slots before eval.
3. `resolveNested` **writes back through `inst.ContextData`**. First read destroys the markers,
   so the next write re-marshals and re-hashes the slot to arrive at the hash it already had.

## Design

### 1. One slot type -- NOT built, and deliberately

The accessor was supposed to need it. It does not: hiding five shapes is what an accessor *is*,
and `Context` hides them behind `At` without any of them changing on disk. Folding
`external_data` and `error_data` onto one shape needs a JSON-restructuring migration across two
engines, which buys tidiness and no behaviour. It stays open, and it is easier after this change
rather than before it, because every reader now goes through one place.

### 2. The context owns the decoded data, a loader and a memo [built]

    type Context struct { data map[string]any; load func(hash string) (any, error); memo map[string]any }
    func (c *Context) At(path ...any) (any, error)

The design expected to compare the requested path against each `Ref.Path`. **No comparison is
needed.** Decoding already places each marker at the path it was cut from, so the three cases
fall out of an ordinary walk that resolves a marker only when it has another step to take:

| the walk | action |
|---|---|
| must step **through** a marker | load it, walk on |
| ends **above** one | return the subtree, marker intact |
| never meets one | load nothing |

Row two is what makes wish 3 work: a caller that copies that subtree copies its markers. The
whole accessor is about thirty lines.

### 3. Paths, not names, in `Roots`

`collectRoots` already walks `MemberNode` chains to recognise `outputs.<id>`. Record the longest
**static** prefix instead, stopping at the first dynamic step (computed key, index, lambda
parameter) and falling back to the enclosing prefix. `AllOutputs` becomes the path `["outputs"]`.
The conservatism rule is unchanged, one level finer.

### 3a. Copy versus read-through -- what actually delivers wish 3 [built]

The refinement the design was missing. Wish 3 needs a marker to survive *into* the evaluated
result, which means `buildEnv` must stop pre-resolving. Doing that naively is a regression:
today `outputs.x.code.length` works because the slot was materialized first, and it would start
finding a marker.

So `collectRoots` gained one bit per root: is this reference **read into or operated on**, or
merely **copied**? The walk already knows -- it descends from a known parent. Navigation
(a field, an index, a computed key), an operator, and a call argument read through; an array
item, an object value and a conditional branch are copy positions. A `${ }` interpolation reads
through (it stringifies); a `$:` expression does not (it hands the value on).

`Roots.Through` is that bit. Copied roots keep their references and are never loaded; roots read
through are materialized exactly as before, so nothing regresses. It is a strictly coarser
analysis than wish 2 and needs none of its machinery.

A side effect worth recording: `error.data` laziness had never worked. The `ErrorData` root
existed and was correct, and `resolveNested` defeated it by materializing every child of the map
it walked, so `error.code` always paid for the body. `Through.ErrorData` restores the intent.

### 4. Resolution is a view, never a write-back

The engine materializes exactly the analysed path set into the expression env; `ContextData`
keeps its markers for the whole advance. Three consequences:

- a slot read once is not re-marshalled and re-hashed by the next write;
- a value nothing read flows into the next write as its reference (wish 3);
- the hash on the write path is the one that came off disk, not one recomputed from a
  round-tripped value — removing a class of churn rather than trusting marshal determinism.

### 5. A marker reaching an operation is an error

Under-reporting the path set today yields `nil`. Under path-level analysis it would let a marker
reach a comparison, a function or a template render, which would compute a *wrong answer*
instead. Add an `*ObjectRef` case to the evaluator's type switches that fails loudly.

This is what makes the analysis safe to refine: copying a marker is legal, operating on one is a
bug, and the bug is visible on the first test that hits it.

## What wish 3 needs from the language

Copying an untouched leaf already works with today's grammar — a shape building
`{b: outputs.x.b}` copies the reference at `b` and the next write re-emits it.

A genuine **partial update of one large object** (`{...outputs.x, y: n}`) does not: there is no
spread and no merge function. That is a language question, not a storage one, and it is the one
part of the target this design does not reach on its own.

### A reference must not cross a boundary [built, after it broke]

Asking "what else should be tested" found a regression rather than a gap. Once an expression can
copy a reference, a parent passing a slot into a child hands it a **marker**, and:

- the child's input is **conformed**, and a conform inspects and normalizes -- it cannot do
  either inside a value it would have to load to see. `expected type string, got *model.ObjectRef`,
  loudly, on a definition that worked the day before;
- the value lands on **another instance's row**, and `applyContextObjectDiff` claimed only
  objects that write had produced -- so the child referenced content it never held, and the sweep
  is entitled to delete content when no claim remains. Silent, and it needs a GC pass to appear.

Two fixes, deliberately both. `evalChildInput` and `child_list`'s `over` materialize at the
boundary (`Engine.concrete`), which is where the rule belongs; and **claims now follow
references, not writes** -- every hash a slot references is claimed, idempotently, whether or not
this write produced it. The first is the rule, the second is what stops the next boundary anyone
forgets from being a silent data-loss bug instead of a loud validation error.

Nothing is lost by materializing there: the child re-cuts the value, content addressing lands it
on the same object, and the result is one object with two claims -- verified, not assumed.

## Non-goals

- The per-slot threshold stays, so a row is still unbounded in the *number* of slots.
- Client-side splicing (genctl, the TS helper, the evaluator worker) is unaffected: those
  consumers hold hashes, not a context.

## Phasing

1. ✅ **Built.** `model.Context` with `At` / `Materialize`, the write-back removed, the
   copy-versus-read-through bit on `Roots`, and the marker-in-operation error. Wishes 1 and 3.
2. **Path-level `Roots`** -- wish 2. Deferred with its fork undecided (static path analysis
   versus lazy values in eval); §3 records the recommendation.
3. **The storage-shape unification** and its migration, now optional rather than a prerequisite.

## Tests, and the one that could not be written where it looked like it belonged

`inst.ResolvedObjects` is the instrument, not the API. **Content addressing makes a copied
reference and a re-loaded, re-hashed one identical on the wire** -- same hash, same objects
section -- so an end-to-end assertion passes whether or not the value was loaded. The first
attempt at `big_values_test.ts` asserted ref equality, passed, and went on passing with the
laziness deliberately broken. The load count only exists in memory, so the test is in Go:
`TestBuildEnv_CopyingASlotNeverLoadsIt` (engine), verified to fail when `through` is forced true.

- `At` loads nothing on a disjoint path, keeps the marker when the walk stops above one, loads
  once when it steps through, and never writes back (`internal/model/context_test.go`).
- `Through` separates copy from read-through per output id, and `error.code` does not pull the
  body (`roots_through_test.go`).
- Copying a marker evaluates; indexing, comparing, interpolating or passing one to a function
  fails and names the object (`external_marker_test.go`).
- `TestBuildEnv_ReadingASiblingLeavesTheBigLeafAlone` pins the DEFERRED half: it asserts one
  load today and is written to fail when path-level laziness lands.

### The matrix, and what building it found

`TestLazyMatrix` (`lazymatrix_test.go`) is one context carrying references at five known places
and a table of expressions over it, each asserting **both axes**: the value produced, and the
exact set of objects fetched to produce it. Either alone is worthless -- the value passes whether
or not a reference was loaded for nothing, and the load set passes if the expression quietly
returns nil.

Every row was checked by breaking the thing it claims to cover. That found two rows whose names
lied and one piece of dead code:

- `{x: "$: outputs.a"}` is a **shape** map, so each leaf is its own template and the
  expression-level `ObjectNode` branch never runs. A row for `"$: {x: outputs.a}"` was needed to
  cover it; both are kept, and named for which layer they exercise.
- `"${outputs.a.code}"` reaches `Through` via the member chain whatever the interpolation bit
  says, so it cannot pin the `${ }` rule. Only a **bare** reference (`"${outputs.whole}"`) does.
- Breaking `Context.At` changed nothing, because `buildEnv` was reading `ContextData` directly
  and calling `Materialize`: the accessor was parallel to the real path, not on it. Routing
  `buildEnv`'s reads through `At` is what made wish 1 true of the engine rather than only of the
  type -- and the same mutation now fails 20 rows.

Beyond the matrix: `child_marker_test.ts` covers all three child types across the boundary above
(each fails when the materialization is removed), and `claims_test.go` pins claims-follow-
references on both engines.
