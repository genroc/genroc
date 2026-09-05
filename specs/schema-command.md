# `genctl schema`: a piece of the process, as a schema

A definition's types exist, and nowhere a user can reach them. Inference computes the type of
every slot and the scope of every expression, and then spends both on error messages. `schema`
makes that view queryable: `context` answers *what can I read where I am writing*, `type`
answers *what shape is this slot* — the second so a piece of the process can be handed to a
code generator, for a client, a consumer, or a worker implementing an `external` task.

## 0. Status

**PROPOSAL 2026-09-04.** Three steps, in order:

1. **The `error` / `last_error` split** — ✅ **BUILT 2026-09-04**
   ([task-scopes.md](task-scopes.md) §The error axis). Not part of this command, and first
   anyway: `error` named two different failures, so a command that reports the scope at a slot
   would have had to document the ambiguity instead of answering. It also decided §3's table.
2. **`context`**, `-e` included — ✅ **BUILT 2026-09-04**, as specced here
   (`internal/validation/slots.go`, `cmd/genctl/schema.go`, `tests/cli/schema_test.ts`).
3. **`type`**, §7 — specced 2026-09-04, once `TaskSchemas` grew the `Result` the inferred view
   did not carry.

It became possible on 2026-09-04, when the types moved into genctl
([source-resolution.md](source-resolution.md) §One roundtrip). Before that this command would
have been a roundtrip per question, which is not a thing anyone runs while writing YAML.

## 1. What it is not

- **Not a verdict.** `validate` says whether a definition is legal; this says what its types
  are. It answers over a document that would be refused, as far as inference gets.
- **Not an editor protocol.** It has no positions and no lenient parse of a half-typed
  expression, so it cannot underlie completion or diagnostics. The consumer is a person or a
  generator, and the reading "this is nearly an LSP" would buy the wrong things: ranges,
  incremental parses, a long-lived process.
- **Not a resolver.** See §5.

## 2. The address

    genctl schema context <process> [address] [-e <expression>] [-f <path|glob> ...]

**The process is a mandatory positional.** Making it optional when the file set holds one
definition was rejected: the single positional then means two things, and which one is decided
by whether it happens to match a process name — a rule that goes wrong exactly for a process
named `output`. Process names are unconstrained (`validate:"required"` and nothing else; only
`config` names have a charset), so there is no lexical rule that separates the two spellings.
One less rule beats one less word.

### The view is one schema, and an address is a path into it

Each view builds **one document**, and an address navigates it — `schema.At`, the same walk that
reads a value's type. There is no address grammar beside it: no arity, no phase resolution, no
slot-versus-navigation boundary, and so nothing that can differ between the two views.

```jsonc
// context                                   // type
{ "output": { /* … */ },                     { "input":  { /* … */ },
  "tasks": { "price": {                        "output": { /* … */ },
    "action":   { /* … */ },                   "raises": { "period_closed": { /* … */ } },
    "output":   { /* … */ },                   "tasks":  { "price": {
    "switch":   { /* … */ },                     "action": { "input": {…}, "result": {…} },
    "on_error": { "0": { /* … */ } } } } }       "output": {…}, "last_error": {…} } } }
```

**`action` stays in the path.** The payload and the result are the ACTION's and sit under it,
exactly where the definition writes them; `output`, `last_error`, `switch` and `on_error` are the
task's and sit beside it. Two namespaces, kept apart by the segment the YAML already has — a
task-level slot added later cannot collide with an action's. It is also what makes
`tasks.<id>.action` answer in BOTH views: what an expression there may read, and what the
action's shape is.

**A slot takes the name the definition gives it**, so an address into the document and the
address of its type are the same string: a fetch's payload is `tasks.<id>.action.body`, every
other action's is `tasks.<id>.action.input`. Only `fetch` diverges, and it is the one action type
no resolver targets — a payload is `input` on child, child_list, child_map and external alike.
That is what makes a manifest pointer a TYPE ADDRESS wherever the slot it names has a type
(source-resolution.md), rather than a second spelling to translate.

**Where a slot has no type, there is no address**, and that is the honest half: `url`, `headers`,
`query`, a switch `case`, an `on_error` clause's `message` hold templates, not contract
boundaries, so `schema type` refuses them by naming what the action does have. Three slots run
the other way — `result` (a fetch's is the accepted `responses`, unioned), `last_error` (which
failures route here) and `raises` (collected from every `raise` clause) — and are DERIVED, so
they have a type and no document path at all. The two spaces coincide where a slot is both, and
neither is a subset of the other.

So `tasks.price` is an object of that task's phases, `tasks.price.output` one of them,
`tasks.price.output.self.result` a walk inside it, and `tasks.price -e 'output'` the same answer
by expression. One logic, all the way down.

**A task id that is not an identifier is quoted** — `tasks["step.one"].output` — because an id is
`required` and nothing else, so a dot in one is otherwise read as a step. It is the same grammar
as the `outputs["step.one"].fee` it addresses, and the rendering is injective, so every address a
listing prints resolves back to itself.

**A rule is keyed, not indexed.** `items` types every element of an array alike, so an array
could not carry a different context per rule; `on_error` is an object keyed `"0"`, `"1"`. Three
spellings reach it — `on_error.0`, `on_error[0]`, `on_error["0"]` — and **the dotted one is
canonical**, because it is the only one a shell leaves alone: zsh reads `[0]` as a glob and
refuses the command with "no matches found" before genctl sees it. Dotting a digit is safe here
and not in `JoinPath`: a bare segment is always a property name, so it round-trips, and an
address is not an expression (`tasks.my task.output` is not one either).

A number is a KEY, so it reads an object and never an array: `tiers.0` on an array is refused
rather than being a second way to index, and `[0]` on an object reads the key of that number only
because indexing an object is otherwise an error. Neither direction conflates anything.

**A miss teaches the space.** The document IS the address space, so what sits at the point of
failure is the list of what could be typed instead: `no "url" in tasks.price, which holds:
action, on_error, output, switch`. A key holding a dot names its quoted spelling, and an address
the OTHER view answers names that view — `tasks.price.result` is a type, not a context.

**What this dropped, deliberately.** `tasks.price.url` used to resolve up to the action phase,
and the answer reported that with an arrow. The rule bought one real thing — `url`, `timeout` and
`body` are one context, and nothing in the YAML says so — and cost another: an address naming
nothing at all (`tasks.send.output.headers`) was answered rather than refused, with the arrow as
the only sign. Navigation refuses it and names the phases instead, teaching the same fact at the
point of the mistake. The arrow is gone with the resolution it reported.

### `-e`: the type of one expression there

    genctl schema context <process> <address> -e 'outputs.price.fee ?? 0'
    genctl schema type    <process> <address> -e 'items[0].sku'

The query with its last step taken: the address selects a schema, `-e` says what an expression
reads against it. A flag and not a third positional, because the address is optional and telling
one from the other needs the rule this section already refused.

**An object schema is a scope** — its properties are the roots — so `-e` is not the context
view's alone, and it is rooted wherever the address stopped: `tasks.price.output -e
'self.result.fee'` and `tasks.price.output.self -e 'result.fee'` are one answer. That is the
whole reason navigation and `-e` belong to both views rather than one each.

**Bare, not the leaf it is written in.** `${…}` belongs to the template layer, where every
interpolated string is `string`; the grammar is unambiguous without it — `total` is a path,
`"total"` a literal. A pasted leaf therefore fails to parse, and the error hands the expression
back unwrapped rather than pointing at the `$`.

At a slot it runs the checker's two phases in the checker's order — **availability, then
inference**. Availability is a statement about the SLOT, so it applies only where the address
stopped at one; once it has walked inside, there is no slot being written and inference answers
alone. A
reference the phase does not carry (`self.result` before the action answers, a previous output no
path returns to, a result the action never types) is refused with `validation.slotRoots`'s own
sentence, which is the one constructor every registration-time check installs; inference alone
would answer "field not found", naming the member and not the rule.

**The third phase is deliberately absent.** The checker then conforms the value against the
slot's required type — boolean for a case, a string for a `url` — and `-e` does not, because an
address names a PHASE and a requirement is per slot: `url`, `timeout` and `children["a"].input`
share one context and require three different things. `-e` answers what an expression produces;
what it must produce belongs to the slot the address deliberately dropped.

The inference is the checker's own too, so at `output` the expression is typed under every arm
and the results joined (§6): `outputs.price.fee` is `number|null` and `?? 0` is not. A declared
`secret: true` travels with the type it sits on — structurally, since the taint that once
followed a secret through a transformation is gone with the redactor it fed
(object-store.md §Redaction). The refusal is half the value: a wrong path gets the checker's
diagnostic with no apply, no server and no resolver.

## 3. The phases

[task-scopes.md](task-scopes.md) owns the model; this command exposes it. **After step 1** the
context varies along one axis, `self`, and everything else is fixed per task: `input`, `config`
and `outputs.*` by which paths reach the task, `last_error` by which tasks route their failure
into it.

| phase | `self` | `error` |
|---|---|---|
| action slots, `timeout` | `previous` | — |
| the output map | `+ result` | — |
| the switch | `+ result`, `+ output` | — |
| `on_error[i]` | `previous` | that rule's declared payload |

The three `self` values are `beforeOutput` / `afterAction` / `afterOutput`, already named in
[scope.go:23](../internal/validation/scope.go#L23). The fourth row is the same `self` as the
first, and is an address rather than a phase name for one reason: `error` is per rule, because
each rule catches a different set of codes.

Before step 1 there was no such table — `error` in the first three rows was the failure that
routed into the task, in the fourth the one that rule caught, and `retry.*` sat in the fourth row
reading the first row's value. Reporting that would have been the alternative to fixing it,
which is why the split was step 1 and not a follow-up. `retry` now reads the fourth row like
every other slot of a rule, so the fourth row is one context and not two.

### What is guaranteed

Every task slot's context is the one the checker built, and not by agreement: **there is one
constructor**. `taskScopes` (`internal/validation/context.go`) holds what a context is built
from and has one method per phase; the checker, `Compare`'s per-task view and `SlotContexts`
all call those, so `contextSchema` has no other caller. Two things make feeding it from a
finished `SchemaFile` sound: inference infers every output in phase 1 before it builds any
phase-2 context, so nothing it used was still being solved, and the pool only ever GROWS
(`uniqueDefName` renames the newcomer), so a ref that resolved during the check resolves the
same way after. `TestSlotContextsAreTheCheckersOwn` pins both — bodies identical, and every
definition the check resolved still resolving to the same thing.

That now covers the process `output` too, whose context is the union the checker types
against. `compat` is a different question entirely: it compares `taskContexts` — the DURABLE row, `config` stripped and no `self` — which
is not what an expression can read. specs/compat-command.md §2a.

## 4. Output

**stdout is the schema and nothing else**, so a piece of a definition pipes into a generator
without a `jq` in between. Diagnostics go to stderr; they never land in the document.

**A schema prints as YAML, and as JSON with `--json`.** YAML is the language definitions are
written in, so an answer can be pasted into one, and it spends no lines on punctuation. Either
way the keys come out in **reading order** — `description`, `$ref`, `type`, the composition
keywords, `properties`, `required`, `items`, the constraints, and `$defs` last — because both
encoders sort a map, which puts `properties` before `type` and the pool before either. An
unrecognised keyword follows, sorted, so a new one shows up rather than disappearing.

The document is **self-contained**: the reachable subset of `$defs`, with `$ref`s rewritten
against the returned root. Inlining is not the alternative — a task output may reference
itself ([recursive-type-inference.md](recursive-type-inference.md)), so refs have to survive.
A definition that is ONLY a `$ref` is dropped, and refs through it name what it named: every
task gets a `<id>_output` because recursion resolves through the name, and where the output
already is a definition that leaves a hop that says nothing.

With **no address** it prints one entry per phase, keyed by address:

```jsonc
{
  "output":                    { /* … */ },
  "tasks.price.action":        { /* … */ },
  "tasks.price.output":        { /* … */ },
  "tasks.price.switch":        { /* … */ },
  "tasks.price.on_error.0":    { /* … */ },
  "$defs":                     { /* … */ }
}
```

The keys are the addresses, so the listing is the map of what can be asked. One entry per
phase and not per slot: the per-slot listing repeats identical contexts dozens of times and
buries the four that differ.

The human rendering names what each slot can READ, rather than printing nine schemas:

```
tasks.price.action       input, outputs
tasks.price.output       input, outputs, self{headers, result, status}
tasks.price.switch       input, outputs, self{headers, output, result, status}
tasks.price.on_error.0   error, input, outputs
```

`self` and `outputs` are spelled out, `?` marks what a path may not set and `=null` what one
state says an ending did not produce, because those are what move; the rest is a root name. A
slot whose context has ARMS — the process output — prints one line per arm, named by it:

```
output   on the path ending at task "left":  input, outputs{left, right=null}
         on the path ending at task "right": input, outputs{left=null, right}
```
 An earlier draft printed a DELTA against the fixed
part (`… +error(limit_exceeded)`), which needs a baseline to diff against and a code name the
schema does not carry — the members are the same information without either.

## 5. No resolver runs

A query must never shell out to `tsc`. It does not have to: inference collapses a literal to
its base type, so an unresolved `$import: ./fee.ts` leaf types as `string` — exactly what the
placeholder `apply` splices types as, by the argument [source-resolution.md](source-resolution.md)
§"Why the placeholder is sound" already makes. The directive is left where it is.

## 6. Open

- ~~**The process `output` context is a floor.**~~ **Settled 2026-09-04 by making the partition
  part of the context**: it is one `anyOf` arm per way the process can end, each naming its
  ending, and inference distributes over the arms. Before that the checker walked the paths
  itself and handed out a flattened context, so `(outputs.a ?? outputs.b) + 1` was refused by a
  reader of the answer and accepted by the checker — the precision existed but was not in the
  artefact. specs/path-sensitive-output.md §2.
- **`child_map` entries** (`children["a"].input`) are per-entry slots sharing the action
  phase. Covered by the resolution rule, so they need no address of their own — until an entry
  gets a scope the others do not.

## 7. `type`

The address space is the **contract boundaries**, the places someone generates code from:
`input`, `output`, `raises.<code>`, `tasks.<id>.action.{input,result}` and
`tasks.<id>.{output,last_error}`. Answers are
standalone documents, as in §4.

### One address space, two questions

`context` and `type` do not have similar addresses; they have **the same** ones. An address
names a slot, `context` says what an expression written there may READ, `type` says what shape
the slot IS, and each refuses — naming its sibling — where it has no answer. That refusal is
what makes it one space rather than two that happen to look alike.

| address | `context` | `type` |
|---|---|---|
| `input` | — | the process input |
| `output` | what the output expression reads | what the process produces |
| `raises["payment.declined"]` | — | that fault's payload |
| `tasks.<id>.action` | the action-phase context | its input and result |
| `tasks.<id>.action.input` | — | what the action is sent |
| `tasks.<id>.action.result` | — | what the action hands back |
| `tasks.<id>.output` | what the output map reads | what the output map produces |
| `tasks.<id>.last_error` | — | the payload of the failure that routed here |
| `tasks.<id>.switch` | the switch context | — (a case is boolean by construction) |
| `tasks.<id>.on_error.<i>` | that rule's context | — |

`tasks.<id>.output` is the case that proves it: one spelling, one slot, and two documents that
answer different questions about it — what an expression written there may read, and what the map
produces. Where only one view has an answer the other names it (§2, a miss teaches the space).

### Navigation

Navigation is §2's, unchanged: this view is a document like the other one, so
`tasks["step.one"].result.tiers[0]` and `raises["http.429"].detail` are paths like any other —
and `tasks.send` is the whole contract of one task, which is what a worker implementor wants.

### `result` is what `self.result` sees [decided 2026-09-04]

A fetch declares `responses` per status, and the answer is the accepted ones, unioned exactly as
inference types `self.result`. Not one address per status: the non-accepted declared bodies are
already `tasks.<id>.last_error` — what routed here carries — so the two halves of the contract each
have an address and the grammar gains no fourth arity. What a worker returns and what a caller
must handle, which is what a generator is being handed.

### The prerequisite is a deletion

`TaskSchemas` carried `ActionType`, `Input`, `Output` and `Error` and no result, so the declared
`result_schema` / `responses` existed only as the inline type of `self.result`. The evidence
that this was a hole rather than a choice: genctl read the declared result **out of the raw
YAML** — `enclosingTask`, [sources.go](../cmd/genctl/sources.go) — to fill the resolver
manifest's `output`, so one half of that manifest was a document read and the other inference.
Adding `Result` is additive (a `SchemaFile` is computed per call, never persisted) and lets that
read go.
