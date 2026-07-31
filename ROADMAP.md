# Planned improvements

## CLI
- [x] commands mirroring the API capabilities
- [x] yaml support
- [x] config file with the url to the server
- [] test with different auth types + add helpers for genctl

## server
- [x] versioning channels
- [x] child process compatibility check and versioning made convenient
- [x] external tasks (outgoing request to start, incoming request to complete), human or long running
- [x] non-idempotent tasks - steps which can't be safely repateated
- [x] logs for each process
- [x] let user to repeat the task manually (how will it interact with parents?)
- [x] pagination
- [x] filtering
- [x] env variables
- [x] map function (lambdas + object/array literals; own parser)
- [x] think about error handling child -> parent (see docs/child-error-handling.md; raise/panic, the `raised` status, error_code, and child→parent catch with batch resolution all implemented)
- [] think about action extensivity/passability from parent
- [x] unknown type - a way how child can pass data to parent, without looking at it (the empty schema `{}`, narrowed by the parent's `result_schema`; no new syntax — see docs/unknown-type.md and examples/polling-task/. The `infer` mode from that doc — inherit the child's computed output — is still open)
- [] fetch response metadata - expose `self.status` and `self.headers` (see docs/fetch-http-surface.md part 1; the status is already on `transport.Response` and dropped one line early. Would retire the `http.202`-via-`on_error` trick in examples/polling-task/ and unblock Location / Retry-After / Link. Additive: both names are registration errors today)
- [] fetch query params - a structured `query` slot (see docs/fetch-http-surface.md part 2; interpolating into the url string does no escaping, so a value with `&`/`=`/space injects a parameter. Null values omit the param)
- [x] string-literal indexing in expressions - `x['some-key']` (unblocks the headers map above. Desugars to a MemberNode, so inference/eval/refs never see the new syntax. What was *not* free: a property key is an arbitrary JSON string, and inference carried access paths as dot-joined strings — so `x['a.b']` and `x.a.b` rendered to the same path, colliding in the narrowing-guard map and, worse, in `SecretAt`, where a secret marked on a dotted key would have navigated to a different node and gone unredacted. Paths are now carried as `[]pathStep` end to end (`nodeSteps`, `stepsHitSecret`), with an explicit index/property discriminator on the step since the empty key `x[""]` is now reachable and was indistinguishable from index 0. The dotted-key leak, the matching over-redaction and the guard collision each have a regression test in internal/expression/expressiontest/string_subscript_test.go. Second half: paths are *rendered* one way everywhere (`internal/schema/path.go` — `JoinPath`/`JoinIndex`/`renderPath`, used by validation errors, shape labels and the guard key), and that rendering is the accessor syntax itself, so a validation error now says `headers["retry-after"]` instead of the unparseable `headers.retry-after`, and `headers["x.y"]` instead of `headers.x.y`, which named the wrong node entirely. `parsePath` reads that syntax back, so a path from a message can be handed to `At`/`SecretAt`; bare dotted paths keep meaning nesting, so authored paths are unaffected)
- [x] Go + REST API error handling (see docs/error-handling-audit.md; the workflow error model was fine — this was the plumbing under it. Errors now carry a `code` on `Reply` (all three transports) which HTTP renders as 400/404/409/501/500, unclassified defaulting to 500; db sentinels (`ErrNotFound`/`ErrConflict`/`ErrInvalid`) so handlers inherit the classification instead of re-deriving it; per-field `fields` on definition-validation failures; `errcode.Code` as a type; `errors.Is` throughout; strict request decoding; and a panic barrier around `advance` that fails the instance with `engine.panic` instead of the worker. Still open: escalation for the log-and-continue background loops)
- [x] look at naming conventions - cancel -> pause, then resume. Retry only for failed processes.
- [] pause as a debugging tool: start an instance paused, then step it with tick

# docs

- [] write docs, plan and motivation


