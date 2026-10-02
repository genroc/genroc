# Documentation site: a reference generated from the code that defines it

Status: **Built, except Pagefind search, the React islands and the per-tag versioned deploy**
(`DOCS_BASE` is wired; no workflow builds tags).

## The gap is genre, not volume

`docs/` is reference and guides for shipped behaviour, for someone using genroc; `specs/` records
decisions. The site never links into `specs/`, nothing is promoted from one to the other, and
guides own the user-level "why" while reference stays free of it.

## Tooling decisions

- **Astro, not Hugo.** Hugo vendors Chroma with no extension points, so a genroc lexer is
  impossible without post-processing HTML. Astro's Shiki loads the TextMate grammar the VS Code
  extension uses (`docs/src/shiki-genroc.ts` reads `editors/vscode/syntaxes/` as-is): one grammar,
  two consumers.
- **No theme**: when the design is the point, a theme is a dependency paid to delete its output.
  Content collections with Zod-validated frontmatter, hand-written CSS, two Shiki themes.

**Search: Pagefind, no service** — post-build over `dist/`, chunked static index, JS
API with our own markup (not the bundled UI); `data-pagefind-body` on content or nav
text pollutes every result. The only JS on reference pages.

**Generated, not written** (`make docs-reference`, `cmd/genrocspec`):

- the CLI reference by **running** `genctl --help`, since its flag sets register only once a
  command executes;
- the REST reference from the OpenAPI document plus `api.Reference()` — permissions and examples
  OpenAPI cannot carry — both read from the action registry, so they cannot disagree;
- the definition reference from `internal/defschema`, and so from the `description:` struct tags;
- errors, config and the instance-status table.

Generators emit **plain MDX**, no framework components, so the pipeline outlives the site's
choices, and generated pages are gitignored. Where a page states rules the code enforces, a test
reads the page (`internal/delayspec/doc_examples_test.go`, `internal/errcode/unknowable_docs_test.go`).

**The editor schema is a static asset.** `make docs-schema` writes `docs/public/process-schema.json`
(with `openapi.json` and `config-schema.json`), served at `genroc.org/process-schema.json`, so a
`# yaml-language-server: $schema=` comment resolves with no genroc running. Generated at deploy,
never committed.

**`docs.yml` has no `paths:` filter**: one would have to list `internal/model`'s whole import
closure, and a miss silently publishes a schema that disagrees with the server. Every push to main
deploys; the Astro build is byte-stable, so an unrelated push changes nothing and the push step
exits without committing.

**Styling**: plain Astro + CSS, ASCII/terminal aesthetic. React islands only where
interaction demands (search, version select, mobile nav, tabs, copy) — Radix
Primitives, because it styles through data attributes (plain CSS against
`[data-state]`, no theme object). Frontmatter deliberately has **no
`status: shipped|proposed`** field — if docs only describe what landed, it has one
legal value; a `since:` field may earn a place instead.

## Navigation direction is derived, not authored

View transitions slide content in the direction the reader went. Each page gets an ordering key
from the nav tree (`00.01.02`, zero-padded, dot-joined): string order is reading order, and a
parent's key prefixes its children's, so one comparison tells *below* from *after*. Equal keys
cancel the navigation and scroll instead. Two digits per level.

**Nesting is the file path**, not a frontmatter `parent`: `a/b/c.mdx` hangs off `a/b.mdx`, so no
page can claim a place its URL contradicts. `order` sorts siblings, and a directory with no page
beside it fails the build rather than dropping out of the nav (`src/lib/nav.ts`).

Traps:

- Fixed chrome (topbar, sidebar, TOC, footer) is captured as named elements, but a named element
  the reader **could not see** flies across the screen when morphed — so each records whether it
  was on screen, and the arriving page neutralises the appeared and disappeared cases.
- **`<link rel="expect" href="#page-end" blocking="render">` is load-bearing**: without it, slow
  delivery either silently drops the transition or animates against a half-parsed blank body. The
  footer id is the contract, and renaming it breaks this silently. Diagnose with
  `document.readyState` and `!!event.viewTransition` at `pagereveal`.
- **Never touch the Navigation API for provenance** — `navigation.activation` throws in Safari.
  The previous path goes through `sessionStorage` on `pagehide`, and the outgoing document tags
  itself from a capture-phase click listener, because its names are assigned while *its* snapshot
  is captured.

## Versioning and deployment

Build per tag into `/v1/`, `/v2/`, latest at root — a workflow, not a plugin (no
framework has this built in). **Build once at the tag, never rebuild**: archived HTML
must not need a three-year-old toolchain. Two traps:

- **Subdirectories, not subdomains**: GitHub Pages allows one custom domain per repo,
  so `v1.genroc.org` needs an archive repo per major or a host move. Reopen if the
  host changes anyway.
- **A `gh-pages` checkout + `rsync --delete --exclude=/bench/`, never `actions/deploy-pages`**
  (`docs.yml`) — the artifact deployment replaces the whole site, silently deleting the benchmark
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
