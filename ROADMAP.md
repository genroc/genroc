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
- [] Go + REST API error handling (see docs/error-handling-audit.md; the workflow error model is fine — this is the plumbing under it: every API error is a 400, no code on the wire, `%w` wrapping nothing unwraps, `err == sql.ErrNoRows`, no panic barrier in advance goroutines)
- [x] look at naming conventions - cancel -> pause, then resume. Retry only for failed processes.
- [] pause as a debugging tool: start an instance paused, then step it with tick

# docs

- [] write docs, plan and motivation


