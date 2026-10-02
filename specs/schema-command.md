# `genctl schema`: a piece of the process, as a schema

Inference computes the type of every slot and the scope of every expression. `genctl schema` makes
that view queryable offline. `context` answers *what can I read where I am writing*. `type` answers
*what shape is this slot*, so a piece of the process can be handed to a code generator (for a
client, a consumer, or a worker implementing an `external` task).

## 0. Status

Built.

## 1. What it is not

- **Not a verdict.** `genctl apply --check-only` says whether a definition is legal; this says what
  its types are. It answers over a document that would be refused, as far as inference gets
  (`newTaskScopes` reads what `Check` managed).
- **Not an editor protocol.** It has no positions and no lenient parse of a half-typed expression.
  The language server ([language-server.md](language-server.md)) adds those over the same APIs.
- **Not a code resolver.** See §5.

## 2. The address

    genctl schema context|type <process> [address] [-e <expression>] [-f <path|glob> ...]

**The process is a mandatory positional.** If it were optional when the files hold one definition,
the single positional would mean two things, decided by whether it happens to match a process name.
Process names are unconstrained, so no lexical rule separates the two spellings.

### The view is one schema, and an address is a path into it

Each view builds **one document** (`ContextDocument`, `TypeDocument`), and an address navigates it
(`Navigate`, via `schema.At`). There is no address grammar beside it, and so nothing that can differ
between the two views. `schema.ParsePath` is the one reader, so do not write a second.

**One exception: a slot address may be a PREFIX of another.** A switch has a whole-switch context
and one per case. A rule has its context, plus one for the `retry` / `panic` / `raise` beside it,
which run only because the rule matched (guard-narrowing.md). The nested document cannot hold both,
since the case index would become a name in scope. So `SlotAt` first resolves the longest slot
prefix from the flat slots, then walks only the remainder inside that slot's schema.

```jsonc
// context                                   // type
{ "output": { /* … */ },                     { "input":  { /* … */ },
  "tasks": { "price": {                        "output": { /* … */ },
    "action":   { /* … */ },                   "raises": { "period_closed": { /* … */ } },
    "output":   { /* … */ },                   "tasks":  { "price": {
    "switch":   { /* … */ },                     "action": { "input": {…}, "result": {…} },
    "on_error": { "0": { /* … */ } } } } }       "output": {…}, "last_error": {…} } } }
```

- **`action` stays in the path.** The payload and the result belong to the action and sit under it;
  `output`, `last_error`, `switch` and `on_error` belong to the task. A later task-level slot cannot
  collide with an action's, and `tasks.<id>.action` answers in both views.
- **A slot takes the name the definition gives it.** A fetch's payload is `tasks.<id>.action.body`,
  and every other action's is `…action.input`. That is what makes a resolver manifest pointer a type
  address (source-resolution.md).
- **Where a slot has no type, there is no address.** `url`, `headers`, a switch `case` and a
  clause's `message` hold templates, not contract boundaries, and `schema type` refuses them.
  `result`, `last_error` and `raises` are DERIVED: they have a type and no document path. Neither
  space is a subset of the other.
- **A key that is not an identifier is quoted** (`["step.one"]`), the same grammar as the
  expression that reads it, and the rendering is injective. Task ids are C identifiers.
- **A switch case is keyed beside the phase** (`switch.0`), because each case reads a different
  context. The bare `switch` address stays: a switch has a whole-switch context before any case
  narrows.
- **A rule is keyed, not indexed.** `on_error` is an object keyed `"0"`, `"1"`, because `items`
  would type every element alike. `on_error.0`, `on_error[0]` and `on_error["0"]` all reach it, and
  **the dotted spelling is canonical**: zsh globs `[0]` and refuses the command before genctl sees
  it.
- **A number is a KEY.** `tiers.0` on an array is refused rather than becoming a second way to
  index. `[0]` on an object reads the key of that number, because indexing an object is otherwise
  an error.
- **A miss teaches the space.** The error lists what is there instead (`no "url" in tasks.price,
  which holds: action, on_error, output, switch`), gives a dotted key's quoted spelling, and names
  the sibling view when that view answers the address.
- **An address naming nothing is refused**, never resolved up to the enclosing phase. Resolving
  `tasks.price.url` to the action phase would also answer `tasks.send.output.headers`, which names
  nothing at all.

### `-e`: the type of one expression there

    genctl schema context <process> <address> -e 'outputs.price.fee ?? 0'
    genctl schema type    <process> <address> -e 'items[0].sku'

The address selects a schema, and `-e` types an expression against it. It is a flag rather than a
third positional, because the address is optional. **An object schema is a scope**, so `-e` works in
both views, rooted wherever the address stopped: `tasks.price.output -e 'self.result.fee'` and
`tasks.price.output.self -e 'result.fee'` are one answer.

- **The expression is bare**, without the `${…}` its leaf wraps it in. That wrapper belongs to the
  template layer, where every interpolated string is `string`. A pasted leaf gets a hint
  (`unwrapHint`).
- **Availability runs before inference**, as in the checker, and only where the address stopped at
  a slot (`CheckSlotRoots`). A reference the phase does not carry is refused with
  `validation.slotRoots`'s own sentence rather than inference's "field not found".
- **The third phase is deliberately absent.** The checker then conforms the value to the slot's
  required type, but an address names a PHASE, and one phase serves slots with different
  requirements (`url`, `timeout`, `children["a"].input`). `-e` answers what an expression produces,
  not what it must produce.
- **The inference is the checker's.** At `output` the expression is typed under every arm and the
  results joined, and a declared `secret: true` travels with the type it sits on.

## 3. The phases

[task-scopes.md](task-scopes.md) owns the model, and this command exposes it. A task has one context
per phase: the action slots (and `timeout`), the output map, the switch (plus one per case), and each
`on_error` rule (plus its clauses). The phases differ in `self`. `error` differs per rule, since each
rule catches different codes.

### What is guaranteed

Every context is the one the checker built, because **there is one constructor**: `taskScopes`
(`internal/validation/context.go`), with one method per phase. The checker, `Compare`'s per-task view
and `SlotContexts` all call it. Building from a finished `SchemaFile` is sound for two reasons.
Inference infers every output before it builds any phase-2 context, and the `$defs` pool only grows
(`uniqueDefName` renames the newcomer). `TestSlotContextsAreTheCheckersOwn` pins it. `compat`
compares something else, `taskContexts` (the durable row, with no `self` and no `config`):
compat-command.md §2a.

## 4. Output

**stdout is the schema and nothing else**, so it pipes into a generator; diagnostics go to stderr.
A schema prints as YAML, the language definitions are written in, or as JSON with `--json`. Keys come
out in **reading order** (`schema.KeywordOrder`: `description`, `$ref`, `type`, composition,
`properties`, …, `$defs` last), followed by unrecognised keywords sorted, because both encoders
would otherwise sort.

The document is **self-contained**: the reachable `$defs`, with refs rewritten against the returned
root. Refs survive rather than being inlined, because a task output may reference itself
([recursive-type-inference.md](recursive-type-inference.md)). A definition that is ONLY a `$ref` is
dropped, and refs through it name what it named (`collapseAliases`). Every task output is stored
as `<id>_output` because recursion resolves through the name, and where the output is already a
definition, that name is a hop that says nothing.

**With no address**, `--json` prints one entry per slot address over one shared pool, so the
listing is the map of what can be asked:

```jsonc
{ "output": {…}, "tasks.price.action": {…}, "tasks.price.output": {…},
  "tasks.price.switch": {…}, "tasks.price.switch.0": {…}, "tasks.price.on_error.0": {…},
  "$defs": {…} }
```

The human rendering names what each slot can read. `self` and `outputs` are spelled out, because
they are what moves. `?` marks a root a path may not set, and `=null` one that a state says an ending
did not produce. A context with arms (the process output) prints one line per arm:

```
tasks.price.output       input, outputs, self{headers, result, status}
output   on the path ending at task "left":  input, outputs{left, right=null}
         on the path ending at task "right": input, outputs{left=null, right}
```

## 5. No code resolver runs

A query never shells out. It runs the structural phase only, because a structural resolver moves the
types reported and a code resolver does not (source-resolution.md §Built-in, and overridable). An
unresolved `$import: ./fee.ts` leaf types as `string`, exactly what `apply`'s placeholder types as
(source-resolution.md §"Why the placeholder is sound").

## 6. Open

- **`child_map` entries** (`children["a"].input`) are per-entry slots sharing the action
  phase. Covered by the resolution rule, so they need no address of their own — until an entry
  gets a scope the others do not.

## 7. `type`

The address space is the **contract boundaries**, the places someone generates code from:

- `input`, `output`, `raises.<code>`
- `tasks.<id>.action.{input|body, query, children.<k>.input, result}`
- `tasks.<id>.{output, last_error}`

Answers are standalone documents (§4). The sent slots are the inferred shape conformed to any
declaration (declared-slot-schemas.md §1).

### One address space, two questions

`context` and `type` have **the same** addresses. `context` says what an expression written at a
slot may READ, and `type` says what shape the slot IS. Each refuses, naming its sibling, where it has
no answer.

| address | `context` | `type` |
|---|---|---|
| `input` | — | the process input |
| `output` | what the output expression reads | what the process produces |
| `raises["payment.declined"]` | — | that fault's payload |
| `tasks.<id>.action` | the action-phase context | its input and result |
| `tasks.<id>.action.input` (`.body` on a fetch) | — | what the action is sent |
| `tasks.<id>.action.query` | — | the fetch query |
| `tasks.<id>.action.children.<k>.input` | — | what that `child_map` entry is sent |
| `tasks.<id>.action.result` | — | what the action hands back |
| `tasks.<id>.output` | what the output map reads | what the output map produces |
| `tasks.<id>.last_error` | — | the payload of the failure that routed here |
| `tasks.<id>.switch` | the switch context | — (a case is boolean by construction) |
| `tasks.<id>.on_error.<i>` | that rule's context | — |

Navigation is §2's: `tasks.price.action.result.tiers[0]` and `raises["http.429"].detail` are paths
like any other, and `tasks.send` is the whole contract of one task.

### `result` is what `self.result` sees

`result` is `TaskSchemas.Result`: a declared `result_schema`, or a fetch's accepted `responses`,
unioned exactly as inference types `self.result`. It is absent where nothing is declared, and on a
routing task. There is not one address per status, because the non-accepted declared bodies are
already `tasks.<id>.last_error`. The two halves of the contract each have an address, and the grammar
gains no fourth arity.
