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
3. **`type`**, §7 — deferred behind a slot the inferred view does not carry, which is a change
   to `validation` rather than to this command.

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

The address is a **path in the expression language's own accessor syntax** — what `JoinPath`
renders and `schema.ParsePath` reads — addressing tasks by **id** rather than index, and indexing
only what has no name:

| address | the context |
|---|---|
| `output` | the process output expression — no `self` at all |
| `tasks.<id>.action` | the action slots, and `timeout` |
| `tasks.<id>.output` | the output map |
| `tasks.<id>.switch` | every case and every clause in the switch |
| `tasks.<id>.on_error[i]` | one rule |

Five forms, and they are the YAML's own keys, so there is nothing to learn. `action.` is an
**optional segment**: `tasks.price.input` and `tasks.price.action.input` are one address.
`switch` takes no index because one context serves every clause; `on_error` takes one because
the error axis is per rule (§3).

**A task id that is not an identifier is quoted** — `tasks["step.one"].output` — because an id is
`required` and nothing else, so a dot in one is otherwise read as a step. It is the same grammar
as the `outputs["step.one"].fee` it addresses, and the rendering is injective, so every address a
listing prints resolves back to itself. It replaced a longest-prefix match over the ids, which
chose between a task `a` and a task `a.b` without saying so.

**Any finer slot resolves to its phase, and the answer names the phase it landed in.** That
resolution is most of the command's value: `tasks.price.url`, `tasks.price.children["a"].input`
and `tasks.price.timeout` are one context, and nothing in the document says so.
[scope.go](../internal/validation/scope.go) `preOutputSlots` is already that enumeration.

### `-e`: the type of one expression there

    genctl schema context <process> <address> -e 'outputs.price.fee ?? 0'

The query with its last step taken: the address says what is in scope, `-e` what an expression
written there produces. A flag and not a third positional, because the address is optional and
telling one from the other needs the rule this section already refused.

**Bare, not the leaf it is written in.** `${…}` belongs to the template layer, where every
interpolated string is `string`; the grammar is unambiguous without it — `total` is a path,
`"total"` a literal. A pasted leaf therefore fails to parse, and the error hands the expression
back unwrapped rather than pointing at the `$`.

It is the checker's own inference, so at `output` the expression is typed under every arm and the
results joined (§6): `outputs.price.fee` is `number|null` and `?? 0` is not. A declared
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
without a `jq` in between. The resolved phase and canonical address are human-mode output or
stderr; they never land in the document.

The document is **self-contained**: the reachable subset of `$defs`, with `$ref`s rewritten
against the returned root. Inlining is not the alternative — a task output may reference
itself ([recursive-type-inference.md](recursive-type-inference.md)), so refs have to survive.

With **no address** it prints one entry per phase, keyed by address:

```jsonc
{
  "output":                    { /* … */ },
  "tasks.price.action":        { /* … */ },
  "tasks.price.output":        { /* … */ },
  "tasks.price.switch":        { /* … */ },
  "tasks.price.on_error[0]":   { /* … */ },
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
tasks.price.on_error[0]  error, input, outputs
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

## 7. `type`, deferred

Recorded so the settled parts are not re-argued. The address space is the **contract
boundaries**, the places someone generates code from: `input`, `output`, `raises.<code>`,
`tasks.<id>.{input,result,output,error}`. Answers are standalone documents, as in §4, and
navigation continues *into* a schema by property — `input.amount` — which is `schema.At`.

The blocker is `result`. `TaskSchemas` carries `ActionType`, `Input`, `Output` and `Error` and
no result, so the declared `result_schema` / `responses` exists only as the inline type of
`self.result`. The evidence that this is a hole rather than a choice: genctl already reads the
declared result **out of the raw YAML** — `enclosingTask`,
[sources.go](../cmd/genctl/sources.go) — to fill the resolver manifest's `output`, so one half
of that manifest is a document read and the other is inference. Adding `Result` to
`TaskSchemas` is additive: a `SchemaFile` is computed per call, never persisted.

Undecided: whether a fetch exposes one `result` (the 200, as `self.result` sees it) or one
address per declared status.
