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
- [] string-literal indexing in expressions - `x['some-key']` (blocks the headers map above: the parser takes only integer literals, so hyphenated keys are unreachable and `self.headers.retry-after` parses as subtraction. Desugars to a MemberNode, so inference/eval are untouched; also unlocks arbitrary-key access on every open map, e.g. config)
- [x] Go + REST API error handling (see docs/error-handling-audit.md; the workflow error model was fine — this was the plumbing under it. Errors now carry a `code` on `Reply` (all three transports) which HTTP renders as 400/404/409/501/500, unclassified defaulting to 500; db sentinels (`ErrNotFound`/`ErrConflict`/`ErrInvalid`) so handlers inherit the classification instead of re-deriving it; per-field `fields` on definition-validation failures; `errcode.Code` as a type; `errors.Is` throughout; strict request decoding; and a panic barrier around `advance` that fails the instance with `engine.panic` instead of the worker. Still open: escalation for the log-and-continue background loops)
- [x] look at naming conventions - cancel -> pause, then resume. Retry only for failed processes.
- [] pause as a debugging tool: start an instance paused, then step it with tick

# docs

- [] write docs, plan and motivation


