# Extension keys

Status: **Built, except what Open lists.**

A key starting with `x-` is the author's own data. Genroc accepts it wherever its own structure is
and never reads it, so a tool that generates definitions keeps its data in them — a builder's
layout, an importer's provenance — rather than in a file beside them. Today every unknown key is
refused (`unknown field "x-builder"`).

## 1. Where

Wherever an object has a SET STRUCTURE — genroc decodes it into one of its own types, with a fixed
set of keys: the definition root, a task, an action, a `child_map` entry, a `switch` case, an
`on_error` rule, `raise`/`panic`, a `retry` or `timeout` object, and any schema node. That type's
decoder skips `x-` among its own keys, and nothing deeper.

Where there is no set structure, every key is loaded as it is today. A shape (`input`, `output`,
`body`, `headers`, `query`, `data`) keeps its value whole, so `headers: {x-api-key}` is a header. A
named map (`properties`, `$defs`, `children`, `responses`, `raises`) takes its keys as names, and
each value under a name is a set structure again:

```yaml
x-builder: { zoom: 2 }       # the definition's own key: skipped, kept in the document
input_schema:
  x-order: 1                 # a schema node's own key: skipped
  properties:
    x-id: { type: string }   # a property name
tasks:
  - id: a
    x-pos: { x: 10 }         # a task's own key: skipped
    output: { x-id: "$: 1" } # inside a shape: data the process outputs
```

The prefix is exactly `x-`, case-sensitive; `x_builder` and `X-builder` stay unknown keys.

The published schema admits them on every object it closes (`patternProperties: {"^x-": {}}`), and
the schema-against-server differential test holds the two to one verdict, one case per position.

A root `x-` key also gives a YAML anchor a place to live (`x-defaults: &d`, merged with `<<`), which
a top-level holder key never had.

## 2. The model never holds them; what is stored is the document

No struct carries an `x-` key, so the engine, validation and compat cannot read one. What an apply
stores is the definition AS SUBMITTED — compacted, key order kept — not the marshal of the decoded
struct.

Every reader decodes the stored document and normalizes it, through one helper: the checks at
apply ran on the normalized form, and the engine resolves a schema's `$ref` into the process
`$defs` only once they are baked in. The definition cache holds the normalized text, so a load
still costs the decode alone.

`GET /definitions/{name}` (`?version=N`, latest by default; permission `read`) returns
`{name, version, definition}`, the definition being the stored document byte for byte, so a tool
reads back exactly what it wrote and can apply it again. OpenAPI types it as a process definition.

Rejected: an extension map on every struct, written back on marshal. It is ten types and the
schema node, and a marshal on the inference hot path, for data nothing reads.

## 3. Versions: the document decides, and the runtime is reported

A version is a document and never changes once written. The content hash is over the document with
keys sorted, plus the child pins: a change to an `x-` key — or to a spelling, `retry: 3` against
`retry: {retries: 3}` — mints a version, and re-applying an earlier document walks the pointer back
to its version.

What RUNS is hashed too, over the normalized model plus the child pins — the basis the content hash
has today — and stored per version. A new version whose runtime hash equals that of the version the
requested channel pointed at reports `same_runtime` (genctl: `latest: p v1 -> v2 (new, same runtime
as v1)`). Its instances behave identically, and `genctl upgrade` moves them with nothing to
reconcile.

The migration adding the runtime hash backfills it from the existing content hash, which was
computed over exactly that basis. The first re-apply of an unchanged file after upgrading therefore
mints one version, reported as the same runtime.

Rejected: versioning by the runtime and replacing a matching version's document in place. A version
would change after it was written, its actor and time describing a document it no longer holds.
Keeping the first document instead drops every later `x-` change in silence.

## 4. Strictness stays the caller's

The door decodes strictly (API, language server); a stored row decodes plainly, because a row
holding a since-removed field must still load (a delay's `ms`, internal/delayspec/CLAUDE.md).

The stdlib does not hand `DisallowUnknownFields` to a type's own `UnmarshalJSON`, and the four types
it decodes today — `ProcessDefinition`, `Task`, `Action`, `ChildEntry` — need one to skip `x-` keys.
Each therefore RECORDS any other unknown key instead of refusing it, and the strict decode refuses a
recorded key afterwards in encoding/json's own words (`json: unknown field "k"`), which the API
passes on and the editor locates the key from. The four decode numbers exactly on both paths; every
consumer of `for`/`until` already takes either form.

The hand-written decoders (`switch`, `on_error`, `raise`/`panic`, `retry`, `timeout`, schema nodes)
refuse unknown keys unconditionally, as today; their allowlists skip `x-`.

Silent when broken: a struct the stdlib decodes INSIDE one of the four is strict on both paths and
refuses `x-` keys. It needs a decoder of its own, and a case in the differential test.

## 5. What does not see them

Compat compares slots, so a bundle differing only in `x-` keys reads `unchanged` there, while
applying it mints a `same_runtime` version. `genctl`'s source resolution treats the document as a
document: a `$<resolver>:` string in an `x-` value resolves like any other, and `$$` escapes it.

## 6. Tests

Each written first and seen to fail, then seen to fail again with its rule removed.

- The schema-against-server differential: a case per position of §1, and the look-alike refused.
- Model: both decode paths accept every position; the document holds every `x-` key and the
  model's marshal none but the output's data key; the door refuses an unknown key at each of the
  four levels while a stored row loads it.
- End to end: stored and returned as written; an `x-`-only change is a new version reporting
  `same_runtime`, each version keeping its own document; an earlier document walks back to its
  version; an `x-` key in an output is data at runtime; `not_found` for an unknown name.
- CLI: the apply line `(new, same runtime as v1)`, `same_runtime` in `--json`, and a root `x-` key
  holding an anchor the tasks merge.
- Language server: no diagnostic for an `x-` key at any position; hover on one, and inside its
  value, says it is an extension key and types nothing, while a header or property name starting
  with `x-` does not; completion offers nothing inside one and never one beside it; a `$:` in one
  is not highlighted as an expression.
- The rest of the CLI: compat reads an `x-`-only bundle `unchanged`; `upgrade` moves an instance to
  a version differing only in `x-` keys; a directive in an `x-` value resolves and `$$` escapes it;
  `schema type` answers offline over a file holding them.
- Migration: its own statements over a table in the pre-migration shape backfill the runtime hash.
- Reading a stored definition without normalizing it fails the existing shared-`$defs` tests.

## Open

- **OpenAPI request types stay closed**, so a typed client casts to send an `x-` key. Admitting
  them there means telling definition components from request bodies the server does refuse
  extras in. Trigger: a client generated from the spec that needs them.
- **A command to read a definition back** (`genctl definitions <name>`). Trigger: wanting one
  outside a tool that talks to the API.
- **A version per save.** A builder applying on every edit mints a version each time. Trigger: that
  noise in practice; the answer is applying on publish, not mutable versions.
