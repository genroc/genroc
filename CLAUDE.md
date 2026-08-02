# genroc

## Comments

Specs live in `docs/`. A comment earns its place only by answering *what would someone
editing this line get wrong* — an invariant, an ordering constraint, a coupling to code
they cannot see from here. If nothing would go wrong, delete it.

- **A self-descriptive function gets no comment at all**, and most functions should be
  self-descriptive — if a name and signature need prose to be understood, rename them
  first. A comment that re-words the name is worse than none: it is a second thing to
  keep true.
- One to three lines. Longer means it is a doc: move it there and leave a pointer.
- Never restate the code, and never re-derive a design `docs/` already argues.
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
| [internal/model/CLAUDE.md](internal/model/CLAUDE.md) | `on_error` validation tiers on an `only_once` task; unknown-key rejection in `on_error` / `switch` |
| [internal/schema/CLAUDE.md](internal/schema/CLAUDE.md) | the `unknown` type (`{}`), why there is no keyword, `NarrowsTo` |
| [internal/delayspec/CLAUDE.md](internal/delayspec/CLAUDE.md) | `delay` syntax (`for` / `until` / `tz`), arity, calendar arithmetic |
| [docs/CLAUDE.md](docs/CLAUDE.md) | which docs are **proposals, not current behavior** |

## Build / test

    make build      # produces ./genroc and ./genctl
    make test       # go test ./... + integration tests

    # Run with SQLite (default):
    ./genroc -db genroc.db

    # Run with PostgreSQL:
    ./genroc -pg postgres://user:pass@localhost/genroc
