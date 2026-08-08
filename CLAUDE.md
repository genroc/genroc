# genroc

## No package-level mutable state

Package-level is fine for **values** — lookup tables, compiled regexes, error sentinels,
embedded files. It is wrong for **state**: if it changes after init, it wants an owner (a
field on the struct whose lifetime it shares).

The reason is Go-specific. A package-level var is reachable from every goroutine and
goroutines are created anywhere, so it must be synchronised whether or not anything is
genuinely shared — and that lock is invisible at the call site. It is also a GC root, which
turns a missing cleanup from a self-correcting bug into a permanent leak, and it creates
dependencies no signature declares.

`internal/archtest` enforces this. A var is flagged when its type mentions `sync`/`atomic`,
or when it is assigned after declaration. The escape hatch is an entry in `allowed` — which
means writing down which owner it could have had and why it could not; `template.cache` is
the worked example.

**A package-level var with a mutex beside it is almost always per-call state that escaped.**
Genuinely shared state comes with an invariant you can name in one sentence; if you cannot
name one, the state is in the wrong place.

## Comments

Specs live in `specs/`. A comment earns its place only by answering *what would someone
editing this line get wrong* — an invariant, an ordering constraint, a coupling to code
they cannot see from here. If nothing would go wrong, delete it.

- **A self-descriptive function gets no comment at all**, and most functions should be
  self-descriptive — if a name and signature need prose to be understood, rename them
  first. A comment that re-words the name is worse than none: it is a second thing to
  keep true.
- One to three lines. Longer means it is a doc: move it there and leave a pointer.
- Never restate the code, and never re-derive a design `specs/` already argues.
- A rejected alternative is worth a clause only when it is the one-line "fix" someone
  would plausibly apply ("do not clear `worker_id` here — it is the evidence
  `ReclaimedExpired` derives from"); the argument itself stays in the doc.
- Exported doc comments are contract, not spec: say what a caller must know, in full.
- In tests the name carries the scenario and the **failure message** carries why it
  matters — a message appears when the test breaks, a comment does not. Comment only
  setup that looks wrong but is deliberate.

## Where the rest of this file went

Per-area invariants live next to the code they govern, so they load only when that code is
in play. **Read the file for the area you are touching before changing it** — each one
records failures that are silent, not compile errors.

| File | Covers |
|---|---|
| [internal/db/CLAUDE.md](internal/db/CLAUDE.md) | sqlc, the dual-engine (SQLite/Postgres) rules, hand-written-SQL exceptions, pagination, Postgres autovacuum bootstrap, adding a query/migration; pause/resume vs retry |
| [internal/engine/CLAUDE.md](internal/engine/CLAUDE.md) | `only_once.interrupted` and the unknowable set at runtime; the live half of lease fencing |
| [internal/api/CLAUDE.md](internal/api/CLAUDE.md) | the action registry as the one place to add an endpoint; shutdown drain ordering, why there is no `WriteTimeout`, the readiness endpoint's independence from the engine, adding a `Code` |
| [internal/transport/CLAUDE.md](internal/transport/CLAUDE.md) | the fetch response cap and how overflow is detected; why the shared client carries no `Client.Timeout`; retry jitter and the exponent clamp |
| [internal/model/CLAUDE.md](internal/model/CLAUDE.md) | `on_error` validation tiers on an `only_once` task; unknown-key rejection in `on_error` / `switch`; `timeout` decoding and its action-type rules |
| [internal/validation/CLAUDE.md](internal/validation/CLAUDE.md) | the version comparison as a conservative floor and the direction a refinement may move; the two `$defs` pools; changed slots as a field comparison; why diagnostics decompose above `isSubset` |
| [internal/schema/CLAUDE.md](internal/schema/CLAUDE.md) | the `unknown` type (`{}`), why there is no keyword, `NarrowsTo` |
| [internal/delayspec/CLAUDE.md](internal/delayspec/CLAUDE.md) | `delay` and `timeout` syntax (`for` / `until` / `tz`), arity, calendar arithmetic |
| [specs/CLAUDE.md](specs/CLAUDE.md) | which docs are **proposals, not current behavior** |

## Build / test

    make build      # produces ./genroc and ./genctl
    make test       # go test ./... + integration tests

    # Run with SQLite (default):
    ./genroc -db genroc.db

    # Run with PostgreSQL:
    ./genroc -pg postgres://user:pass@localhost/genroc
