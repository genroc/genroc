# Declared slot schemas: the slot's published type, conformed at the boundary

An optional schema may sit beside a shape: `input_schema` beside a child or external `input`,
`body_schema` and `query_schema` beside a fetch's, and `output_schema` beside a task's output and the
process's own. Where one is written it is that slot's **public type**. The editor completes against
it, `$process` spreads it and the version comparison reads it. The value is conformed to it on the
way out, so the declaration is true of what left.

The point is **import**, not expressiveness. A schema imported from an OpenAPI document
([openapi-resolver.md](openapi-resolver.md)), a child definition
([source-resolution.md](source-resolution.md) §`$process`) or a worker fleet makes a call checkable
offline, with no database and without the other side being registered.

## 0. Status

Built, except what §11 lists.

## 1. Thesis: where a declaration exists, it is the type

> A declared schema is the slot's public type. The value is conformed to it before it leaves
> the slot, so what the declaration says is what left. Where no declaration exists, nothing
> changes and the inferred type remains the only answer.

A slot type is computed **once**, in `validation` (`TaskSchemas`), and `genctl schema type`, the
resolver manifest and hover all read it. A second rule in any consumer is how two answers to one
question came about. The rule is asymmetric:

- **A slot the definition HANDS BACK** (a task output, the process output) publishes its declaration
  (`published`). That is the contract consumers read, and §3 keeps it honest.
- **A slot the definition SENDS** (a body, a query, an input, a `child_map` entry's input) is typed
  by what actually leaves it: the inferred shape conformed to the declaration (`sent`, using
  `schema.Conformed`). It is not the declaration alone, which is the far side's contract; that made a
  generic child's `input: {}` read as unknown where a script's argument type is generated from. Nor
  is it the raw inferred type, which still carries the nulls the conform removes.

Conforming makes a declaration true by construction, so no consumer has to decide whether to believe
it. It also does bookkeeping an author cannot do by hand (§4). The conform is an **assertion**, not
a check (§4). Adding a declaration changes what a slot publishes, so it is a version event (§9).

**A declared task output goes back into the pool as written, after `Solve`** (`outputorder.go`).
The solver stores canonical forms, which drop `description`, the prose a declaration is imported
for. It is restored only where the check passed: a failed slot keeps the `{}` the diagnostics'
suppression expects.

## 2. The slots

| slot | shape | target without a declaration |
|---|---|---|
| `tasks.<id>.action.input_schema` | child / `child_list` / external `input` | the child's `input_schema` at registration, or nothing |
| `tasks.<id>.action.children[k].input_schema` | that entry's `input` | same |
| `tasks.<id>.action.body_schema` | fetch `body` | nothing |
| `tasks.<id>.action.query_schema` | fetch `query` | `object` of scalar-or-array-of-scalar, nullable |
| `tasks.<id>.output_schema` | task `output` | nothing |
| `output_schema` | process `output` | nothing |

**`child_list` has no `input` shape.** Each element of `over` is one child's input, so the
declaration types **one element**, like `result_schema` does there. The check runs against `over`'s
item type (`checkDeclaredListElement`), and the conform runs per element. Checked against the absent
`input`, it would compare an empty object and assert nothing. An `over` with no declared item type
is refused by name.

**Placement is per action type** and refused by name elsewhere (`validateInputSchemaPlacement`).
`body_schema` and `query_schema` are fetch-only. `input_schema` is refused on a fetch, where the
name is `body_schema`, and on a `child_map` action, where it is declared per entry.

## 3. The check is CLOSED, and the conform is why that is a choice

`IsSubset` lets the sub side carry a property super never declares. That is right where a conform
strips extras at the boundary. These slots have a conform too, so the open relation would **silently
delete** a key the author wrote, such as a misspelled `pgae=2`. Therefore **a key the declared schema
does not declare is refused.** The conform's strip stays for keys nobody wrote.

The relation is the `closed` flag on `subsetMode`, not a walk beside the relation: a parallel walker
rediscovers unions, `$ref` cycles and open maps badly
([internal/schema/CLAUDE.md](../internal/schema/CLAUDE.md)). Its break kind is `BreakUndeclared`.

**It must read sub's `additionalProperties`, not only its `properties`.** An inferred type can be an
open map (`object<string>`, a `child_map`'s output), whose values carry keys no schema names. An
open-map sub against a closed super is refused at every depth. Without that arm the strip stays
reachable and §4's assertion is silently false, while every table of declared properties still
passes.

**`additionalProperties` in a declared schema is refused, at any depth**
(`Schema.CheckNoAdditionalProperties`, called from model validation's `checkDeclaredSlotSchema`).
Admitting it would make the closed rule conditional on a keyword. It is a per-slot rule, not in
`CheckDoc`, because the keyword is valid in a `result_schema`. Refusing is the reversible direction:
it costs an author a projection today and can be relaxed to exactly the existing open behaviour on
the day the argument arrives.

## 4. The conform, and the null the author should not have to think about

The engine applies the declared schema with `ConformToSchemaExactly` (`conformDeclared`) before the
value leaves the slot.

The motivating case is an optional, non-nullable property (`discount: {type: number}`) fed `null` by
a `??` chain that ran out. `{"discount": null}` does not satisfy the schema, but absence does, and
there is no filter builtin to drop the key by hand. The conform's removal rule reconciles it by
removing the key. The rule does not fire on a **required** property (neither state is valid), on an
**array element** (dropping it shortens the array), or where the target is **also nullable** (both
states are valid). So the declaration is how an author says which they mean: `type: number` drops
the null, and `type: [number, "null"]` sends it. `query` already omitted a null parameter, so this
generalises an existing rule.

### The relation must accept exactly what the conform closes

> A relation that tolerates more than the fill can close promises a migration that then fails
> to conform; a fill that closes more is dead code.
> ([internal/schema/CLAUDE.md](../internal/schema/CLAUDE.md))

`ConformsExactlyTo` is `{absentAsNull, nullRemoval, closed}`. It covers the insert half (a null
written into an absent required nullable) and the removal half, and **no defaults rule**, because
`ConformToSchemaExactly` never fills a default. That is why `nullRemoval` is split from
`afterConform`, and why this is a fourth relation rather than a flag on `IsSubsetAsStored`
(`TestConformsExactlyToHasNoDefaultsRule`). The pairing is pinned in
`schematest/conforms_exactly_test.go`.

### Where it runs, and what it costs

| slot | conform point |
|---|---|
| process `output` | at completion, before the value is stored |
| task `output` | when the output map is evaluated, before it becomes `outputs.<id>` |
| fetch `body`, `query` | before the request is built, so a dropped null is never serialised |
| child / external / `child_list` element / `child_map` entry `input` | before the payload is handed over, and **before** the child's own `ValidateInput`, which stays the child's boundary |

A task output is conformed and then read by a process output that is conformed again, so the
conform must be idempotent. `conform_exact_test.go` asserts it.

### This conform is an ASSERTION, and that is the whole reason it is safe

A value arriving from **outside** (a fetch response, a child's output, a worker's submission) is
unknown until it arrives, so conforming it is a **check** with a legitimate failure, and
`result.invalid` is catchable. Every slot in §2 holds a value **computed here** from values already
conformed, by expressions the checker typed, and §5's relation proves the value fits the
declaration. So **the conform cannot fail, and a failure is a bug in genroc**: unsound inference, a
relation accepting a gap its fill cannot close, or a conform defect. It is never a condition in the
author's data. Three things follow:

1. **Unknowns stay refused** (§5). An unknown really can be anything at runtime, which would make a
   failure legitimate.
2. **A declaration may not narrow** (§8), by the same argument from the other end.
3. **The failure is uncatchable.** A catchable one would be routed by an author's `on_error`, and
   the bug it exists to reveal would be hidden.

The codes are the terminal `engine.*` family, which `failInstance` handles and `on_error` never
routes. Inputs use `engine.input`, which is already this assertion for a child's input failing its
`input_schema`. Outputs use `engine.output`. A fetch's request side (`body`, `query`) folds into
`engine.input` rather than earning a code of its own (`declaredFailureCode`). **It is not a Go
panic**: a worker advances many instances, and a terminal code crashes only this one, loudly.

**The unrepairable case is unreachable.** A required non-nullable property holding null cannot be
fixed, and the removal rule is gated on the property being optional, so the relation refuses that
gap statically. Likewise §3 refuses an undeclared key at registration, so the strip has nothing left
to remove.

## 5. What each check compares

**The relation is `IsSubset`, closed, plus §4's null rules, and NOT `NarrowsTo`.** `NarrowsTo` admits
an unknown on the value side because a runtime conform stands behind it, and it is the one-line
"fix" someone will reach for to allow `body: "$: outputs.x"` over an unknown `x`. It would make
every conform failure ambiguous between a genroc bug and an author's untyped value, which is the
distinction the design rests on. *An unknown flowing into a typed input is rejected on purpose.*

**Child input runs two different checks.**

- `inferred ⊆ declared` runs **closed** and needs no database. It is the first input check the
  editor and an offline genctl can run at all, since `ValidateChildProcessRefs` needs a
  `DefinitionGetter`.
- `declared ⊆ child.InputSchema` runs at registration, **open** (`checkDeclaredAgainstChild`). The
  child's schema is not ours to close.

**Where a declaration exists, the registration check REPLACES the inferred-vs-child one.** The
inferred type still carries the nulls the conform removes, so keeping both checks refuses a call
that works at runtime. The `$process` copy of `input_schema` makes this registration check a test
that the copy is still current (source-resolution.md §`$process`).

## 6. `query_schema` has a target above it

A query value is a scalar, null, or an array of scalars (`querySchema`), and a declaration may not
widen that. `checkDeclaredQuery` checks the declaration against that target **first**, so a bad
declaration is reported as one, rather than later as a shape doing what it was told. Here the
conform is unobservable: a null is omitted either way, §3 refuses undeclared keys, and there is no
nesting to repair. It stays for uniformity, and its e2e test pins the two null rules agreeing.

## 7. What the editor does with it

A declared schema is an author's type in a KEY position, which editor completion had never had.
Typing inside a declared `body` offers the fields the endpoint accepts, with their types and prose.

- **Key completion** (`declaredKeys`) is consulted in `keysAt` before `legalKeys`. It offers the
  declared properties not yet written, required first. The generated language schema cannot absorb
  this ("this mapping's keys come from a sibling's value, possibly via a file" is not JSON Schema),
  so it is a second source beside `processSchema`.
- **Hover on a key** (`shapeKeyHover`) fires on the key span only, and reads the type view and
  nothing else, so it cannot disagree with the CLI (`TestKeyHoverIsTheCLIsOwnAnswer`). It must read
  the PARENT and take the member, because `Schema.At` reads an optional property as nullable. It
  must also follow the `$ref` a slot's type is stored behind.
- **Inside a `$:`** the scope is answered as everywhere else.
- **Completion reads the declaration directly**, because it answers what MAY be written there (the
  far side's contract), not what is.

**The declaration is read from the resolved definition (`definition()`), never from `Doc`.** A
`$process` spread supplies one with no node in the document's index. `Doc` gives the cursor its
path. Like `raises`, the answer comes from THIS document and never from another buffer
([internal/lsp/CLAUDE.md](../internal/lsp/CLAUDE.md)). **Closedness makes the list authoritative**:
§3 refuses anything else, so the diagnostic catches exactly what completion failed to prevent.

## 8. Publishing, hiding, and being more specific

A declared `output_schema` is what `$process` spreads and what the comparison reads (§9), and §4
makes publishing it a statement about what left.

**Hiding is refused.** A declaration omitting keys the output produces (expose `{status}` while
computing `{status, debug}`) would have the conform strip them, so §3 refuses the undeclared key
instead: we decline to delete what someone wrote. Admitting `additionalProperties` (§11) is what
would reopen it, and it should be reopened deliberately rather than as a side effect.

**A declaration may widen and may resolve an unknown, but may not narrow.** An author who knows a
field is `enum: [sent, failed]` where inference says `string` is refused. A narrowing declaration is
a claim the value side cannot prove, so the conform would gain a legitimate failure. What a
declaration can already add is what a public API most needs and inference cannot produce:
`description`, stable names, and the optionality the author means. The appetite to narrow is mostly
literal types, which [literal-types.md](literal-types.md) would supply by inference. Do not weaken
the relation to buy it, for example by accepting any declaration not provably disjoint: that trades
a registration failure for a process that runs to completion and then cannot deliver.

## 9. The seams, and what is silent when broken

- **`Shape.Conformed` picks the relation** (`Shape.fits`), and the slot sets it. The fixed targets
  (`headers`, an undeclared `query`, `accepted_status`) keep the open relation and no conform.
  Flipping one is a behaviour change to a shipped slot.
- **The version comparison reads the published type.** `inferred` fits `declared`, so adding a
  declaration **widens** what a process produces, which compat-command.md's direction rule reports
  as a contract event, once. After that the process may refactor freely inside it.
- **The upgrade gate's floor rule still binds** ([internal/validation/CLAUDE.md](../internal/validation/CLAUDE.md)):
  nothing here may turn a tolerable verdict into a refusal.
- **The editor schema is per variant.** `actionSchemaTemplate` makes each action variant
  `additionalProperties: false`, so a new `_schema` key needs an entry in every variant it belongs
  to, or the editor refuses what the server accepts.
- **Diagnostics point at the shape, not the schema.** A closed break is reported at the shape's slot
  address, with the sub-field as its location. Only a malformed declaration reports at the
  `_schema` slot. Getting this backwards underlines the imported document when the call site is
  what is wrong.

## 10. Tests

- `schematest/conforms_exactly_test.go` pins the relation against the conform in both directions.
  It also asserts the assertion's own property: a conform behind an accepted pair never strips a
  non-null key (`TestConformsExactlyToNeverStripsANonNullKey`). `conformed_test.go` pins
  `Conformed` to the type the fill produces.
- `tests/integration/declared_schemas_test.ts` covers the runtime half. The null repair needs all
  three cases (an optional slot loses the key, a required slot still fails, a nullable target keeps
  the null), or it passes by doing nothing. Every slot kind is conformed.
- `tests/lsp/declared_schemas_test.ts` covers the offline half: closed refusals, `additionalProperties`,
  `child_list` elements, completion including a declaration that arrived by `$process` spread
  (§7's `Doc`-versus-`definition()` trap), and key hover per slot. `tests/lsp/agreement_test.ts`
  and `TestKeyHoverIsTheCLIsOwnAnswer` hold the editor and the CLI to one answer.

## 11. Open

- `headers_schema`. Headers have a fixed target (`object<string>`), so it would add only
  required-ness. Trigger: an importer's request side to fill it.
- `additionalProperties`, and with it hiding (§3, §8). Trigger: a count, not a debate. The refusal
  names the keyword and slot, so running an importer's request side over real documents counts
  the cases (the responses already translate it: [openapi-resolver.md](openapi-resolver.md) §3).
- Narrowing declarations, if [literal-types.md](literal-types.md) does not remove the appetite
  (§8). The measurement that decides it is how many real declarations want to say something
  inference will never produce once literals are inferred.
- Whether `$process` spreading `input_schema` should be a **required** part of the spread or an
  entry an author may drop. It is the only one of the four that can be checked against its
  source, so dropping it is more visibly a choice than dropping `raises`.
- A code action filling every missing required key from the declaration. The obvious companion
  to an import; out of scope because it is an edit, and every answer in §7 is a read.
