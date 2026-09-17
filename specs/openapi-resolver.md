# `$openapi`: an operation's response types, spread into a fetch

The same idea as `$process`: a structural resolver pre-fills a call site from the definition of
the thing being called. There the definition is another process and the site a child task; here
it is an OpenAPI operation and the site a `fetch` action. What comes across is the **typing** of
the call — its method and its status-keyed response schemas — never the request, which stays the
author's, and never a `url`, which a path template is not.

## 0. Status

**PROPOSAL 2026-09-17.** Nothing built. It needs nothing unbuilt: it registers beside `process`
([source-resolution.md](source-resolution.md) §Built-in, and overridable), the merge is the
spread's unchanged, and the editor gets it through the one resolution seam
([internal/lsp/CLAUDE.md](../internal/lsp/CLAUDE.md)). Estimated at two days, most of it §3 and §4.

## 1. The directive

```yaml
action:
  type: fetch
  <<: "$openapi: ./api.yaml#getUser"
  url: "${config.api}/users/${input.id}"
```

The fragment names the operation: an `operationId`, or `GET /users/{id}` for a document without
them. It is required — a document is not an operation. Suffixes `.yaml`, `.yml`, `.json`; the
`#…` is not part of the suffix, which is one generic change to `matchResolver`. The spread must
sit in a `fetch` action, and anywhere else is refused by name rather than left to surface as
`unknown field "responses"` on a child.

## 2. What it fills

`method`, and `responses`: status → the `application/json` body schema. `4XX` becomes `4xx`; a
response with no JSON body becomes `null`, which accepts the status and types nothing; a
`$ref` into `components/responses` is followed. An explicit key beats the spread, as everywhere.

**`default` is dropped.** genroc has no catch-all status: `accepted_status` and the `responses`
keys are the whole statement of what is expected, and a `default` mapped to `5xx` would claim a
shape for statuses the operation never named.

**Not filled, and why.** `url`: `/users/{id}` is a template, and where `id` comes from is the
author's knowledge, not the document's. The request side: genroc's `query`, `headers` and `body`
are values, not schemas, and what is sent is the server's to judge
(docs: validation-and-types). `accepted_status`: implied by the 2xx keys.

## 3. The dialect

genroc's schema language is a strict allowlist ([schema.go](../internal/schema/schema.go)),
and every schema this resolver emits must decode through it — that allowlist is the test
oracle. Each OpenAPI keyword is one of three things:

| | keywords |
|---|---|
| **translate** | `nullable: true` → `"null"` added to `type` (3.0), or an `anyOf` with `{type: null}` where there is no `type` to add to (a `$ref`); `const: x` → `enum: [x]`; `additionalProperties: true` → `{}`, `false` → absent; `#/components/schemas/X` → `#/$defs/X` |
| **strip** | `format`, `pattern`, `title`, `example(s)`, `xml`, `externalDocs`, `deprecated`, `readOnly`, `writeOnly`, `discriminator`, `uniqueItems`, `multipleOf`, `minProperties`, `maxProperties`, `exclusiveMinimum`, `exclusiveMaximum`, `x-*` |
| **refuse**, naming the operation and the path | `not`, `if`/`then`/`else`, `patternProperties`, `propertyNames`, tuple `items`/`prefixItems`, `unevaluated*`, `dependent*`, any `$ref` that is not into `components/schemas` or `components/responses` |

Stripping a bound is safe in the one direction a response schema can err: it accepts more. An
absent `additionalProperties` means *open* in OpenAPI and *strip undeclared keys* in genroc, and
both land as absent — extra fields in a response are dropped, which is what a fetch does today.

## 4. `allOf`

The crux, because OpenAPI spells inheritance with it — `allOf: [{$ref: Base}, {properties}]` is
what every generator emits — and genroc refuses the keyword on purpose: *navigation cannot
resolve a member through an intersection, so it would be a half-supported keyword*
([schema.go:11](../internal/schema/schema.go#L11)). It is `&`, not `|`. On object types the
intersection is the obvious merge, and the resolver performs it:

- a `$ref` arm is followed into the pool and its target flattened first; a cycle is refused;
  a `$ref` arm with siblings other than `description` is refused
- an arm constraining nothing (`{}`, or annotations alone) is skipped
- every other arm must be an object schema carrying only `type`, `properties`, `required`,
  `additionalProperties`, `description` — an `enum`, a `default`, a bound has no meaning as an
  intersection and is refused, not dropped
- `properties`: union; the same name in two arms must be identical, else refused
- `required`: union
- `additionalProperties`: carried over; two arms opening the object differently are refused
- a nullable object arm is refused: `(A | null) & B` is `A & B` in every type system, which is
  rarely what was meant, and the whole schema can be nullable instead
- the node's own keys beside `allOf` are arm zero

Three things make `&` cheap in TypeScript and do not carry over, and they are the cost: there
is no bottom type, so a contradiction is refused rather than typed `never`; navigation is
eager, so the merge must have run before anything looks; and JSON Schema objects can be closed.

**In the resolver, not the language.** One entry point; every refusal is the resolver's message;
the resolver is the only producer of an `allOf` in genroc, so it is reversible; and whether
*authors* may compose schemas in a definition is a language question to decide on its own merits,
not as a side effect of importing a document. The merge is a pure function over schema documents
(`flattenAllOf(doc, pool)`), written so it can move into `canonical.go` unchanged if that day
comes. Rejected for now: distributing `allOf` over a `oneOf`/`anyOf` arm — refused instead.

## 5. Ordering and cycles

No spread cycle: an OpenAPI document cannot reach a definition. A recursive schema in
`components` stays a named `$def` (`selfContained` / `unwrapRootRef` already keep the ref that
is load-bearing); a cycle *through* `allOf` is refused, since it has no finite merge.

## 6. Tests, and the one measurement to take first

Unit: the §3 table, one case per row; the §4 rules, one case per refusal; every emitted schema
through `schema.Parse(…).CheckDoc()`. End to end, mirroring `tests/cli/spread_test.ts`: a 3.0 and
a 3.1 fixture, `genctl schema type … tasks.<id>.action.result` showing the merged properties,
one `apply`, and one editor hover on `self.result` — one, because the seam is one.

**Before building §4's fallbacks: run the flattener over the real documents in hand and count
the refusals.** If inheritance covers them, this is the design. If real documents lean on
closed bases and redefined properties, that count is the argument for `allOf` in the language,
and the argument should be made with it.

## 7. Open

- `url` when the path has no parameters — fill from `servers[0].url` + path, or never.
- Two 2xx responses with different bodies: how `self.result` types across two 2xx keys is
  genroc's existing rule, not this resolver's — pin it with a test rather than assume it.
- `nullable: true` on a `$ref` becomes an `anyOf` wrapper; acceptable, or refuse.
- Remote and multi-file `$ref`s: declined for now, refused with the path.
