# Documentation site: a reference generated from the code that defines it

Status: **partly built (2026-08-03).** The Astro scaffold lives in `docs/` (content
collections with Zod-validated frontmatter, hand-written CSS, two Shiki themes,
direction-aware view transitions, three seed pages; `make docs` / `make docs-build`).
Unbuilt, and still intent below: Pagefind, the generators, the genroc TextMate grammar,
the React islands, versioned deployment. The existing reference page is a hand-written
stand-in.

## The gap is genre, not volume

~350 KB of prose, none of it user-facing **reference**: nowhere to look up what
`accepted_status` accepts or what `genctl promote` does — the answers live in struct
tags and half-proposal specs. Hence the `docs/`→`specs/` rename: a directory called
docs whose contents are half proposal is a trap.

**The split is by what the text asserts.** `specs/` records decisions (why chosen, what
rejected, what unsettled; free to hold dropped ideas). `docs/` records shipped behavior
in the present tense. Consequences: the site never links into `specs/`; nothing is
"promoted" (a landed feature gets documentation written against shipped behavior, the
spec stays put); and `docs/` carries its own explanation — guides own the user-level
"why", reference stays free of it.

## Tooling decisions

Surveyed the field on four axes (custom grammar, service-free search, full styling
control, component model). Findings that settled it:

1. **Hugo loses on one unfixable point**: it vendors Chroma with no extension points,
   so a genroc-flavoured lexer is impossible without post-processing HTML. Astro uses
   Shiki, which loads a TextMate grammar from a file — the same file
   `editors/vscode/` will need. One grammar, two consumers.
2. **A theme is worth negative value when the design is the point** — paying a
   dependency to delete its output.
3. One candidate carried a bus factor of 1 — weighed, recorded, decided nothing (exit
   cost of a docs site is low).

**Search: Pagefind, no service** — post-build over `dist/`, chunked static index, JS
API with our own markup (not the bundled UI); `data-pagefind-body` on content or nav
text pollutes every result. The only JS on reference pages.

**Generated, not written**: `cmd/genrocschema` → field reference (the `description:`
tags are already maintained prose nobody reads); `openapi.json` → API reference;
`genctl --help` → CLI reference. Generators must emit **plain MDX**, no framework
components, so the pipeline outlives this page's choices. Snippets get the
`examples/` treatment — a test fails when they drift.

**The editor schema is published as a static asset** (built 2026-08-21).
`cmd/genrocspec -schema` writes `docs/public/process-schema.json` and the site serves it
at `genroc.org/process-schema.json`, so a `# yaml-language-server: $schema=` comment
resolves with no genroc running — the failure that motivated it. Generated at deploy
time, never committed: a committed copy is a second thing to keep true.

Which is why **docs.yml carries no `paths:` filter**, against the obvious saving. A filter
would have to list every package the schema reflects — `internal/model` and its whole
import closure — and the failure when it misses one is silent: no deploy, and a published
schema that disagrees with the server until someone notices. Deploying on every push to
main costs a runner minute and removes the list. It stays quiet because the Astro build is
byte-stable, so an unrelated push produces an identical `dist/` and the push step exits
before committing.

**Styling**: plain Astro + CSS, ASCII/terminal aesthetic. React islands only where
interaction demands (search, version select, mobile nav, tabs, copy) — Radix
Primitives, because it styles through data attributes (plain CSS against
`[data-state]`, no theme object). Frontmatter deliberately has **no
`status: shipped|proposed`** field — if docs only describe what landed, it has one
legal value; a `since:` field may earn a place instead.

## Navigation direction is derived, not authored

View transitions slide content, and the slide must agree with where the reader went.
Each page gets an ordering key from the nav tree (`00.01.02`, zero-padded,
dot-joined): lexicographic order matches reading order, and prefix testing
distinguishes *below* from *after* — four directions from one comparison; reordering a
page moves its slide with it. Equal keys cancel the navigation and scroll instead.
Hard-won details:

- Fixed chrome (topbar/sidebar/TOC/footer) is captured as named elements, but a named
  element the reader **could not see** flies across the screen when morphed — so each
  records on-screen-ness, and the arriving page neutralises appeared/disappeared cases.
  Visibility, not existence, is what makes one rule cover a missing sidebar and a
  below-the-fold footer.
- **`<link rel="expect" href="#page-end" blocking="render">` is load-bearing**: slow
  delivery otherwise either silently drops the transition (missed paint deadline) or
  animates against a partially-parsed blank body. Inlining/prefetching only narrow the
  race; the expect link names the requirement. The footer id is the contract — renaming
  it breaks this silently. Diagnose with `document.readyState` +
  `!!event.viewTransition` at `pagereveal`.
- **Never touch the Navigation API for provenance** — `navigation.activation` throws in
  Safari on contact, killing the handler. Previous path goes through `sessionStorage`
  on `pagehide`; the outgoing document tags itself from a capture-phase click listener,
  because its names are assigned while *its* snapshot is captured.
- Limits: two digits per level; the nav map is inlined per page (free at this size).

## Versioning and deployment

Build per tag into `/v1/`, `/v2/`, latest at root — a workflow, not a plugin (no
framework has this built in). **Build once at the tag, never rebuild**: archived HTML
must not need a three-year-old toolchain. Two traps:

- **Subdirectories, not subdomains**: GitHub Pages allows one custom domain per repo,
  so `v1.genroc.org` needs an archive repo per major or a host move. Reopen if the
  host changes anyway.
- **`peaceiris/actions-gh-pages` with `keep_files: true`, never `actions/deploy-pages`**
  — the artifact deployment replaces the whole site, silently deleting the benchmark
  time series bench.yml pushes under `bench/`, which exists nowhere else.
- Assets referenced relatively — root-absolute links break the moment a build lands
  at `/v1/`.

## Not scoped: a playground

Not planned; recorded only because the architecture need not anticipate it — an island
is additive (one component, one WASM asset, one worker). The fact that makes it cheap
if ever wanted: **`internal/validation` has no db/engine/api dependency** — the thing
that would run in the browser is a wrapper, not a port.

## Still open

Where guide-level "why" stops and spec-level "why" begins (the first guides will set
it). Versioning mechanics (per-tag build is a sketch; the switcher needs a manifest).
Whether one TextMate grammar really serves both Shiki and VSCode (rendering vs bracket
matching/folding) — worth knowing before the grammar is written twice.
