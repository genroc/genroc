# genroc

A durable process orchestrator. You describe a process as a set of tasks in YAML
(or JSON); genroc runs each instance to completion, surviving worker crashes,
restarts, and long waits without holding a thread or losing state.

Every task checkpoints to a database before and after it runs, so an instance can
be picked up by any worker at any time. Long-running work — polling a remote job,
waiting on a human, backing off between retries — parks in the database and holds
no worker while it waits.

## What it gives you

- **Crash-safe execution.** Instances are leased to workers; a crashed worker's
  lease expires and another worker resumes exactly where it left off. A worker that
  merely *stalled* — a suspended laptop, a throttled container — notices its leases
  went unrenewed, repairs them before claiming, and holds off taking over its peers'
  (see [specs/lease-fencing.md](specs/lease-fencing.md)).
- **At-most-once tasks.** A task marked `only_once` is never re-run by the engine
  after an interruption. Instead it raises `only_once.interrupted`, which `on_error`
  can catch — so the definition asks the system of record what actually happened and
  then carries on, or re-runs the task deliberately. Retries are refused outright for
  the errors nothing came back from (see
  [specs/only-once-interrupted.md](specs/only-once-interrupted.md)).
- **Structural control flow.** Tasks route with `switch` (`next` / `end` /
  `$goto` / conditional cases). There is no `while`/`until` — loops are expressed
  by routing back to an earlier task, which keeps every iteration a crash-safe
  checkpoint (see [examples/polling-task](examples/polling-task)).
- **Child processes.** A task can spawn keyed (`child_map`) or fan-out
  (`child_list`) child processes and wait for them, with versioning and
  compatibility checks between parent and child.
- **External tasks.** A task can hand off to a human or a long-running external
  system (`external`) and resume when the result is signalled back in.
- **Pause / resume / retry.** A running tree can be suspended and resumed with
  nothing else changed — timers keep running, so it carries on exactly where it
  stopped. Retrying is the separate, deliberate act of granting a *failed* tree
  an attempt its definition did not authorise (see
  [specs/pause-resume.md](specs/pause-resume.md)).
- **Typed data flow.** Process input, task outputs, and child results are
  described with a strict JSON-Schema subset, and output types are *inferred* —
  including recursive shapes (see [specs/recursive-type-inference.md](specs/recursive-type-inference.md)).
- **Config vars & secrets.** Per-process / global config is read from the
  environment (`GENROC_<PROCESS>_<NAME>`, `GENROC_GLOBAL_<NAME>`); values marked
  `secret` are redacted from logs.
- **Versioning & channels.** Definitions are versioned; named channels (e.g.
  `latest`) point at a version and can be promoted.
- **Per-instance logs**, pagination, and filtering across the API.
- **Two storage engines.** SQLite (single file, default) or PostgreSQL
  (production, concurrent workers) — same SQL, chosen at startup.
- **Bounded by default.** A fetch response is capped (a body past the limit raises the
  catchable `output.too_large` instead of taking the worker down with every lease it
  holds), request bodies and connection timeouts are capped, retry backoff is jittered so
  a recovering endpoint is not hit by the whole backlog at once, and `GET /healthz` is a
  readiness probe that answers 503 when a worker cannot reach its database (see
  [specs/resource-limits.md](specs/resource-limits.md)).

## Binaries

| Binary       | Purpose |
|--------------|---------|
| `genroc`     | The server: runs the engine and serves the API over HTTP / TCP / Unix socket. |
| `genctl`     | Command-line client for a running server (apply, run, inspect, logs, pause/resume/retry), inspired by kubectl. |
| `genrocspec` | Emits the server's OpenAPI spec (`openapi.json`). |

## Quickstart

```sh
make build            # produces ./genroc and ./genctl

# Run the server with SQLite (default):
./genroc -db genroc.db

# ...or with PostgreSQL:
./genroc -pg postgres://user:pass@localhost/genroc
```

The server listens on `:8448` by default (`-http`, `-tcp`, `-uds` to configure).
Point `genctl` at it with `GENROC_SERVER` (default `http://localhost:8448`).

Define a process — a minimal `greet.genroc.yaml`:

```yaml
name: greet
input_schema:
  type: object
  properties:
    url:  { type: string }
    name: { type: string }
  required: [url, name]
tasks:
  - id: call
    action:
      type: fetch                       # an HTTP call; every field is templated
      url: "${ input.url }/hello"       # ${ } interpolates into a string
      body:
        greeting: "Hello, ${ input.name }"
      result_schema:
        type: object
        properties:
          ok: { type: boolean }
        required: [ok]
    output: "$: self.result"            # $: is one typed expression, not a string
    switch: end
```

Apply and run it:

```sh
genctl apply -f greet.genroc.yaml
genctl run greet --set url=https://api.example.com --set name=World
genctl get @last          # inspect the most recent instance
genctl logs @last         # its per-instance logs
```

See [examples/polling-task](examples/polling-task) for a fuller example — a parent
that spawns a child process which polls a remote job until it finishes or exhausts
its attempt budget, and narrows the opaque payload it hands back.

## Development

```sh
make build      # build (runs sqlc first)
make test       # go unit tests + TypeScript integration tests
make run        # build and run locally
make swagger    # regenerate openapi.json via genrocspec
```

Persistence is split between **sqlc-generated** queries (from
`internal/db/queries.sql`) and a small set of hand-written dual-engine queries.
All SQL must compile against both SQLite and PostgreSQL — see
[CLAUDE.md](CLAUDE.md) for the database conventions (adding a query, adding a
migration, the dual-engine rules). Run the DB tests against Postgres with:

```sh
POSTGRES_DSN=postgres://user:pass@localhost/genroc go test ./internal/db/...
```

## Layout

```
cmd/           genroc (server), genctl (CLI), genrocspec (OpenAPI)
internal/
  engine/      the poll/lease/advance loop and task actions
  db/          persistence (sqlc-generated + hand-written dual-engine SQL)
  numeric/     exact base-10 numbers: decode, compare, format
  model/       process definition & instance types, wire encoding
  schema/      JSON-Schema subset: normalize, validate, type inference
  validation/  definition validation, context/dataflow analysis
  expression/  the expression language ($: typed leaf, ${ } interpolation)
    syntax/    its grammar: AST + parser (expr-lang's lexer, our grammar)
  template/    splitting ${ } out of strings, parsed once per template
  transport/   outgoing request transports (HTTP/TCP)
  api/         HTTP handlers, action registry, OpenAPI reflection
  logview/     log formatting (basic / detail / json)
tests/         TypeScript end-to-end integration tests
specs/         design docs and specs
```

## Benchmarks

<https://genroc.github.io/genroc/bench/>
