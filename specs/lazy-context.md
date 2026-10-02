# Lazy context access

**Built, except wish 2 (§Open).** The read side of
[object-store.md](object-store.md): a caller asks the context for a path, and only what that path
needs is loaded.

## The target

1. **One accessor.** A caller asks the context for a path. Whether `outputs.x` is inline, cut in
   three places, or a single whole-slot reference is the context's business, not the caller's.
2. **Laziness at the path, not the slot.** Not built — §Open.
3. **Untouched means unloaded.** A value an advance never reads must reach the next write as the
   reference it already was — no load, no re-hash, no new object.

Wish 3 needs nothing from the write path: `cutForSize` already re-emits an `*ObjectRef` leaf as
the reference it is, with no new object.

## Design

### 1. One slot type [built]

Every value column holds its value, and references live in one `objects` list per owner
(object-store.md §Every owner declares its references). `error_internal` needs no shape of its
own: reading `last_error.code` without loading the body is `Context`'s job.

### 2. The context owns the decoded data, a loader and a memo [built]

    type Context struct { data map[string]any; load func(hash string) (any, error); memo map[string]any }
    func (c *Context) At(path ...any) (any, error)

Decoding places each marker at the path it was cut from, so an ordinary walk needs no comparison
against `Ref.Path`; it resolves a marker only when it has another step to take:

| the walk | action |
|---|---|
| must step **through** a marker | load it, walk on |
| ends **above** one | return the subtree, marker intact |
| never meets one | load nothing |

Row two is what makes wish 3 work: a caller that copies that subtree copies its markers.

### 3. Paths, not names, in `Roots`

Not built — §Open.

### 3a. Copy versus read-through [built]

`buildEnv` must not pre-resolve a value the expression only copies, yet `outputs.x.code.length`
must still find a value. So `collectRoots` records one bit per root, `Roots.Through`: is it
**read into or operated on**, or merely **copied**? Navigation (field, index, computed key), an
operator and a call argument read through; an array item, an object value and a conditional branch
copy. `${ }` reads through (it stringifies); `$:` copies (it hands the value on). Copied roots keep
their references and are never loaded; read-through roots are materialized. Conservative:
over-reporting costs only a load, under-reporting hands an operation a marker (§5).

The same bit makes `error.data` lazy: `Through.ErrorData` / `Through.LastErrorData` let a handler
read `error.code` without loading the body.

### 4. Resolution is a view, never a write-back [built]

The engine materializes exactly the analysed set into the expression env; `Context.Data()` keeps
its markers for the whole advance. So a slot read once is not re-marshalled by the next write, a
value nothing read flows on as its reference, and the hash on the write path is the one that came
off disk rather than one recomputed from a round-tripped value.

### 5. A marker reaching an operation is an error [built]

Copying a marker is legal; computing on one would give a plausible wrong answer. The evaluator
refuses it (`checkResolved`), naming the object — a hit means `Roots` called a read a copy, an
engine bug. This is what makes the analysis safe to refine.

## What wish 3 needs from the language

Copying an untouched leaf already works with today's grammar — a shape building
`{b: outputs.x.b}` copies the reference at `b` and the next write re-emits it.

Known limitation: a **partial update of one large object** (`{...outputs.x, y: n}`) has no
spelling — there is no spread or merge — so it rewrites the whole object.

### A reference must not cross a boundary

Once an expression can copy a reference, a parent passing a slot to a child would hand it a
marker. The child's input is **conformed**, which cannot inspect a value it would have to load;
and the value lands on **another instance's row**, which would reference content it never claimed
— silent data loss once the sweep runs. Two fixes, deliberately both:

- `evalChildInput` and `child_list`'s `over` materialize at the boundary (`Engine.concrete`) — the
  rule;
- **claims follow references, not writes**: every hash a value references is claimed, idempotently
  — so the next forgotten boundary is loud, not data loss.

Materializing costs no storage: the child re-cuts the value onto the same object, now with two
claims.

## Non-goals

- The per-slot threshold stays, so a row is still unbounded in the *number* of slots.
- Client-side splicing (genctl, the evaluator worker) holds hashes, not a context.

## Tests

Content addressing makes a copied reference and a re-loaded, re-hashed one **identical on the
wire**, so an end-to-end test passes whether or not anything was loaded. The instrument is the
in-memory load count (`inst.ResolvedObjects`), so these are Go tests:

- `TestBuildEnv_CopyingASlotNeverLoadsIt` — fails when `through` is forced true.
- `internal/model/context_test.go` — `At` on a disjoint path, above a marker, through one; never
  writes back.
- `roots_through_test.go`, `external_marker_test.go` — copy versus read-through; operating on a
  marker fails and names it.
- `TestBuildEnv_ReadingASiblingLeavesTheBigLeafAlone` pins the DEFERRED half: it asserts one
  load today and is written to fail when path-level laziness lands.
- `TestLazyMatrix` asserts **both axes** per expression — the value and the exact set of objects
  fetched; either alone passes a broken implementation. Its rows are named for the layer they
  exercise: `{x: "$: outputs.a"}` is a shape map (each leaf its own template), `"$: {x: outputs.a}"`
  the expression `ObjectNode`; only a **bare** `"${outputs.whole}"` pins the `${ }` rule, since a
  member chain reaches `Through` regardless.
- `child_marker_test.ts` covers all three child types across the boundary; `claims_test.go` pins
  claims-follow-references on both engines.

## Open

- **Path-level laziness (wish 2).** Reading `outputs.x.y` still loads the 200 KB leaf at
  `outputs.x.code`: `Roots` is name-level and `buildEnv` resolves whole slots. Recommended: record
  the longest static prefix in `collectRoots`, stopping at the first dynamic step (`AllOutputs`
  becomes `["outputs"]`), over lazy values in eval. Build when sibling reads beside a large leaf
  show up in load counts; `TestBuildEnv_ReadingASiblingLeavesTheBigLeafAlone` fails when it lands.
