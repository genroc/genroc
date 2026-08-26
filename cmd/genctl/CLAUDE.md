# cmd/genctl

## Command conventions

Keep new list/get commands consistent so the surface stays predictable.

- **Naming.** A collection is the plural noun (`instances`, `external-tasks`); a single
  item takes its id/key as the first positional (`get <id>`). Add a `get` only when there
  is something to show beyond the row.
- **Server & errors.** Every command takes `--server` (overrides `$GENROC_SERVER` and the
  config file). All failures go through `fatal()` ("genctl: …"); surface a server-side
  validation message via `serverErrorDetail` / `resultValidationError`.
- **List output.** A tabwriter table with an UPPERCASE header and `shortTime()`
  timestamps; print "no \<things\>" when empty. Filters are `--<field>` flags mapped 1:1
  to the endpoint's query params.
- **List bounds — no `--limit`.** Fetch through `fetchOrdered[T](url, limit, dir, emit)`:
  pass `listCap` and rows arrive as the newest N flipped to oldest-first; pass 0 and they
  stream forward page by page. `applyWindow` sets the query's `*_after`/`*_before` bounds
  and returns which to pass, so naming where to begin is the one way past the cap. Always
  report the capped result through `noteCapped` — **a cap nobody can raise must never
  truncate silently.**
- **Single-item output.** A `Key:\tvalue` tabwriter block with `longTime()` timestamps.
- **The assertion commands take an id list; the reads take exactly one.**
  `pause`/`resume`/`retry` act on every id named through `eachInstance` — one call each,
  each answer under its own id, and only a *refusal* fails the command. An id already in
  the asserted state prints `already` and exits 0, so a line that half applied is repaired
  by running it again; `get`/`logs` refuse a second positional rather than dropping it.
  specs/id-list-commands.md.
- **An instance id may stand in for a process name.** `upgrade` and `compat` take either in
  the same positional, told apart by shape (`isInstanceRef`: a UUID, or `@last`). An id names
  one row, so the flags that SELECT rows — `--from`, `--status` — are refused there rather
  than ignored: a selector that is silently overridden reads as if it applied.
- **`--json` is the one machine-readable form.** A list prints raw items as a JSON array
  via `printJSONItems` (lossless, same order as the table); a single item prints the raw
  server object (`callGet` into `json.RawMessage`, then indent). Never invent a
  per-command machine format.

Deliberate exceptions — special-purpose, not resource list/get. Leave them:

- `logs` keeps `--mode basic|detail|json`: three views, and its json is JSONL (one object
  per line, streaming), not a `{items, page}` array. It also caps at 200 rather than
  `listCap` and renders as it streams — the others build a tabwriter, which sizes its
  columns from every row and so cannot.
- `definitions` offers `--sort name`, and is the one list whose capped read keeps the
  **first** N rather than the newest (`fetchOrdered`'s `firstFirst`). `--since`/`--until`
  still bound `created_at` under it, as filters over the window rather than the point the
  walk starts from.
- `channel list` prints plain `name -> vN` pointer lines (a projection, not a resource
  object), and `status` is a coherence report, not a listing.

## Exactness is the whole path

genctl was the last lossy hop for large numbers, in three places — YAML upload, response
display, and `--set` — each of which decoded into an `interface{}` through float64. See
[specs/number-precision.md](../../specs/number-precision.md); `yamlnum.go` is the walker
that keeps a literal exact, and `tests/cli/genctl_precision_test.ts` asserts on raw stdout
because parsing it in JavaScript would corrupt the values under test.

## YAML merge keys

`yamlToAny` walks mappings itself, so YAML's `<<` is not free — and getting it wrong is
silent: the alias lands under a literal `"<<"` field, the server drops it as unknown, and
the canonical re-marshal strips the evidence. Explicit keys beat merged ones (YAML's own
precedence). The **sequence form is refused**: YAML 1.1 gives its *earlier* entries
precedence, the reverse of every other merge, so `<<: [*base, *override]` would silently do
the opposite of what it reads as.

**Anchors must hang off a real node** — `action: &defaults` on the first task that uses the
shape. A top-level holder key (`_defaults: &d`) is rejected, because `ProcessDefinition`
decodes with unknown fields disallowed. A reserved, genctl-stripped key would fix that; it
is unbuilt, and is the same shape as `$import`-as-a-key in
[specs/source-resolution.md](../../specs/source-resolution.md).
