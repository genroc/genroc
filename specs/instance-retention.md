# Instance retention

Status: **Built.**

`--instance-retention` (off by default, `0`) deletes a process tree once its root concluded (`completed`, `failed`, `raised`, `cancelled`) longer ago than that. A deleted instance
answers 404 everywhere, so results must be read and failures retried within the window. Off by
default because deleting is the one thing an operator must not discover after the fact; the
production guide recommends setting it.

## 1. The unit is the tree

Never an instance alone: a parent reads its children's rows at collect, and retry rebuilds a tree
from them. The root's `updated_at` is when it concluded — nothing writes a concluded root but
retry, which un-concludes it — so the search is a partial index on concluded roots
(`idx_instances_concluded_roots`) and needs no new column. A tree with any unsettled row is
skipped, though the engine concludes a root last.

## 2. What goes with it, and what does not

One transaction per batch deletes the tree's `process_signals`, `process_logs` (by `root_id`) and
`process_instances`. Object claims are not deleted here: `CollectObjects` releases an `instance`
claim whose owner row is gone, as it already did for `log` claims, and the content follows after
`--object-grace`. Driven by absence, a crash between the two is repaired by the next sweep.

## 3. Read unlocked, re-check under the lock

Candidates are read without a lock, then every row of those trees is locked in id order —
`lockTree`'s global order, or Postgres deadlocks against a concurrent retry — and each tree is
re-checked before the delete. A single `DELETE … WHERE root_id IN (subquery)` would re-check only
each row after waiting, not the subquery, and delete a tree a retry had just revived: the same trap
as the object sweep (internal/db/CLAUDE.md, the object store, item 2).

## 4. Batches

A pass runs beside the log prune and deletes at most 10 batches of 200 trees, each its own
transaction, so a backlog — the first start after enabling it — drains over passes instead of
stalling the pump that runs it.

## Open

- **A longer window for failures** — when someone needs to retry or inspect failures older than
  the rest; a second flag, read only for `failed` roots.
