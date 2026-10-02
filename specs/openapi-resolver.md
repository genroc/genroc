# `$openapi`: an operation's response types, spread into a fetch

Like `$process`, a structural resolver pre-fills a `fetch` from an OpenAPI operation.

## 0. Status

**Proposal**, needing nothing unbuilt: it registers beside `process`
([source-resolution.md](source-resolution.md) §Built-in, and overridable). Trigger: the first real
document to import.

## 1. The directive

`<<: "$openapi: ./api.yaml#getUser"`, valid only in a `fetch` (elsewhere refused by name). The
fragment is required, since a document is not an operation: an `operationId`, or `GET /users/{id}`.
Suffixes `.yaml`, `.yml`, `.json`; the `#…` is excluded from suffix matching (one change to
`matchResolver`).

## 2. What it fills

`method`, and `responses`: status → the `application/json` body schema; `4XX` becomes `4xx`, no
JSON body becomes `null`, `components/responses` refs are followed. `default` is dropped: mapped to
`5xx` it would claim a shape for statuses never named. `url` is never filled; a template's
parameters are the author's.

The request side may fill `body_schema` and `query_schema`, but must **refuse** `pattern` and
`format` there, not strip them. §3's strip is safe only for a response: on a request a stripped
bound passes a value the server rejects, and the conform cannot catch what its schema lacks.

## 3. The dialect

Every emitted schema must decode through genroc's allowlist (`allowedKeywords`,
[schema.go](../internal/schema/schema.go)), which is the test oracle.

| | keywords |
|---|---|
| **translate** | `nullable: true` → `"null"` added to `type` (3.0), or an `anyOf` with `{type: null}` where there is no `type` to add to (a `$ref`); `const: x` → `enum: [x]`; `additionalProperties: true` → `{}`, `false` → absent; `#/components/schemas/X` → `#/$defs/X` |
| **strip** | `format`, `pattern`, `title`, `example(s)`, `xml`, `externalDocs`, `deprecated`, `readOnly`, `writeOnly`, `discriminator`, `uniqueItems`, `multipleOf`, `minProperties`, `maxProperties`, `exclusiveMinimum`, `exclusiveMaximum`, `x-*` |
| **refuse**, naming the operation and the path | `not`, `if`/`then`/`else`, `patternProperties`, `propertyNames`, tuple `items`/`prefixItems`, `unevaluated*`, `dependent*`, any `$ref` that is not into `components/schemas` or `components/responses` |

Stripping a bound is safe for a response: it accepts more. Absent `additionalProperties` is open in
OpenAPI and strips in genroc; either way extra response fields are dropped, as a fetch does today.

## 4. `allOf`, flattened in the resolver

OpenAPI spells inheritance `allOf: [{$ref: Base}, {properties}]`, and genroc refuses `allOf`
because navigation cannot resolve a member through an intersection
([schema.go](../internal/schema/schema.go#L72)). On objects the merge is obvious, so the resolver
performs it (`flattenAllOf(doc, pool)`): the language is unchanged, and the choice reversible.

`$ref` arms flatten first, and a cycle is refused. An arm may carry only `type`, `properties`,
`required`, `additionalProperties`, `description`. `properties` and `required` union. Refused, not
dropped: any other keyword, a property differing between arms, arms opening `additionalProperties`
differently, a nullable arm (`(A | null) & B` is `A & B`), and a contradiction (no bottom type).

**The trigger to revisit is the refusal count on real documents.** If they lean on closed bases
and redefined properties, that count is the argument for `allOf` in the language.
