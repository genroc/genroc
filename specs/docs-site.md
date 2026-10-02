# Documentation site: a reference generated from the code that defines it

Status: **Built, except Pagefind search, the React islands and the per-tag versioned deploy**
(Open).

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

**Styling**: plain Astro + CSS, ASCII/terminal aesthetic. Frontmatter has **no `status:`** field:
docs describe only what landed, so it would have one legal value.

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

## Deployment

**A `gh-pages` checkout + `rsync --delete --exclude=/bench/`, never `actions/deploy-pages`**
(`docs.yml`) — the artifact deployment replaces the whole site, silently deleting the benchmark
time series bench.yml pushes under `bench/`, which exists nowhere else.

## Open

- **Pagefind search** — when the nav stops finding pages. Post-build over `dist/`, no service, our
  own markup over its JS API; `data-pagefind-body` on nav text pollutes every result.
- **React islands** (Radix, styled through `[data-state]`) — only where interaction demands it:
  search, a version select.
- **Per-tag versioned deploy** — `/v1/`, latest at root, when a second major ships; `DOCS_BASE` is
  wired, no workflow builds tags. **Build once at the tag, never rebuild**: archived HTML must not
  need an old toolchain, which is why Swagger UI is vendored rather than loaded from a CDN.
