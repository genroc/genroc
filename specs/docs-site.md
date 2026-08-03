# Documentation site: a reference generated from the code that defines it

Status: **partly built (2026-08-03).** The rename `docs/` → `specs/` freed `docs/` for the
site, which is why every path in the repo now points at `specs/`. The Astro scaffold now
lives in `docs/`: content collections with a Zod-validated frontmatter schema, the
hand-written CSS, the two hand-written Shiki themes, direction-aware view transitions
(§ *Navigation direction is derived*), and three seed pages (two guides, one reference).
Built with `make docs` / `make docs-build`.

Still unbuilt, and everything below still describes intent rather than behavior: Pagefind,
the generators (§ *Generated, not written*), the genroc TextMate grammar, the React islands,
and the versioned deployment. The reference page that exists is hand-written and is a
stand-in for generated output.

## The gap is genre, not volume

The project has roughly 350 KB of prose and no user-facing documentation. What exists:

| Artifact | Genre | Reader |
|---|---|---|
| [README.md](../README.md) | pitch + quickstart | someone deciding whether to try it |
| `specs/` (21 files) | design records, **half of them proposals** | the author, later |
| `internal/*/CLAUDE.md` | invariants at the point of edit | someone editing that package |
| [examples/](../examples/) READMEs | four worked patterns, executable as tests | a user, along four paths |
| `openapi.json`, `cmd/genrocschema` | machine-readable surface, unrendered | nobody, currently |

The hole in the middle is **reference**. There is nowhere to look up what
`accepted_status` accepts, which fields a `child_list` action takes, how `on_error`
matches a code pattern, or what `genctl promote` does. Today the answer lives in
[internal/model/definition.go](../internal/model/definition.go) and, for half the
questions, in a spec that may describe something unbuilt.

That is also why `specs/` needed its own name. A directory called `docs/` whose contents
are half proposal is a trap that [specs/CLAUDE.md](CLAUDE.md) currently defuses with a
warning — a warning that works only because the reader is an agent who was told to read it.

## `specs/` and `docs/` are different kinds of statement

The split is not by polish or by audience. It is by what the text asserts.

**`specs/` records decisions.** Why a design was chosen, what was rejected and on what
argument, what is still unsettled. It is internal and stays internal — its usefulness
depends on being free to hold ideas that were dropped, and half of it describes behavior
that does not exist.

**`docs/` records behavior.** What the system does, in the present tense, for someone
using it. Nothing proposed, nothing historical, no argument. A feature is documented when
it ships, not when it is designed.

Three consequences, and the third is the one that is easy to get wrong:

- **The site never links into `specs/`.** A reference page that outsources its "why" would
  send a reader to a document that may argue for something unbuilt.
- **`specs/` is not a draft of `docs/`.** Nothing gets promoted. When a feature lands, its
  documentation is written against the shipped behavior; the spec stays where it is,
  answering a different question.
- **`docs/` therefore carries its own explanation.** Users need "why" too — why loops are
  expressed by routing rather than `while`, why `only_once` raises instead of retrying.
  That belongs in **guides/**, written at user level and independently of the argument the
  spec makes. **reference/** stays free of it.

So: **reference/** is the contract — what the definition language accepts, what the HTTP
API exposes, what `genctl` does, largely generated (§ *Generated, not written*).
**guides/** is task-oriented and explanatory, seeded from `examples/` and extended to cover
config vars, versioning and channels, and running against Postgres.

## The tooling survey

Surveyed before choosing, on the four things this project actually needs:

| | Custom grammar for snippets | Search without a service | Styling is ours | Component model |
|---|---|---|---|---|
| mkdocs-material | Pygments lexer, plugin | ✅ built-in | fights the theme | ❌ |
| Hugo + theme | ❌ **Chroma is vendored, no extension points** | Pagefind, post-build | ✅ total | ❌ shortcodes only |
| Docusaurus | ✅ Shiki/Prism | ✅ local plugin | fights the theme | ✅ React |
| Nextra | ✅ Shiki | ✅ Pagefind (v4) | opinionated | ✅ React |
| Fumadocs | ✅ Shiki | ✅ Orama, static | ✅ component-level | ✅ React |
| Astro + Starlight | ✅ Shiki | ✅ Pagefind | fights the theme | ✅ islands |
| **Astro, no theme** | ✅ Shiki | ✅ Pagefind | ✅ total | ✅ islands |

Three findings settled it.

1. **Hugo loses on one specific, unfixable point.** It is otherwise the best fit — the
   blog at `github.com/stepan662/blog` is hand-written Hugo layouts, 16 KB of plain CSS,
   zero client-side JavaScript, which is exactly the intended aesthetic. But Hugo vendors
   Chroma with no extension points ([gohugoio/hugo#10421](https://github.com/gohugoio/hugo/issues/10421)),
   so a genroc-flavoured YAML lexer is impossible without post-processing the built HTML.
   Astro uses Shiki, which loads a TextMate grammar from a file — the same format the
   still-empty `editors/vscode/` will need. One grammar, two consumers.
2. **A theme is worth negative value when the design is the point.** Starlight's and
   Fumadocs' contribution *is* their design system; replacing it with an ASCII/terminal
   aesthetic means paying a dependency to delete its output. This is the whole argument
   against them and it has nothing to do with their quality.
3. **Fumadocs carries a bus factor of 1.** 5,121 commits from the author against 12 from
   the next human contributor, funded by individual GitHub Sponsors. Present maintenance
   is excellent — releases every two to three days, 2 open issues against 842 closed — and
   the exit cost for a docs site is low. Recorded because it was weighed, not because it
   decided anything.

## Search: Pagefind, and no service

A hard requirement, not a preference: **no Algolia, no external index, no API key.**
Pagefind runs as a post-build step over `dist/`, reads the HTML that was just produced, and
writes a chunked static index — the browser fetches only the fragments a query touches,
rather than downloading the corpus like a naive JSON index would.

Use the **JS API, not the bundled UI**: `pagefind.search(q)` returns results, the markup is
ours. The site's search box is then styled like everything else. Mark the content region
`data-pagefind-body` and exclude the sidebar, or navigation text pollutes every result.

This is the only JavaScript the reference pages carry.

## Generated, not written

Hand-write only what cannot be derived. Three sources already exist and are currently
unrendered:

- `cmd/genrocschema` → the field-level reference. The `description:` struct tags in
  [definition.go](../internal/model/definition.go) are already maintained prose that nobody
  reads.
- `make swagger` → `openapi.json` → the HTTP API reference.
- `genctl <cmd> --help` → the CLI reference.

Anything generated cannot drift. The corollary is a constraint on the generator: **it must
emit plain MDX**, not framework components, so that the pipeline outlives any decision on
this page.

The same discipline applies to snippets. [examples/README.md](../examples/README.md)
already promises that a README drifting from its YAML fails the suite; reference snippets
should be applied by a test the same way. It is the only mechanism that keeps a language
reference true while the language moves.

## Styling and components

Plain Astro, plain CSS, ASCII/terminal aesthetic. React islands appear only where
interaction demands them — search overlay, version select, mobile nav, in-content tabs,
copy buttons — so React ships on the pages that have those and nowhere else. Sidebar, TOC,
topbar and every content page stay static `.astro`.

**Radix Primitives** for those islands. Not for its adoption (though `@radix-ui/react-select`
is at 218M downloads/month against Headless UI's 27.7M, and Headless UI has not been pushed
since 2026-04-13), but because Radix styles through data attributes:

```css
.select-item[data-highlighted] { background: var(--fg); color: var(--bg); }
```

Plain CSS against `[data-state]`, no Tailwind, no theme object to override — the same way
`style.css` already works on the blog.

Frontmatter is validated by a Zod schema on the content collection, so a page missing a
title or an ordering key fails the build rather than rendering wrong. Note what is *not* in
that schema: there is no `status: shipped | proposed`. Earlier drafts of this document had
one, which was a symptom of not yet having drawn the line above — if `docs/` only ever
describes what landed, the field has one legal value. What may earn a place instead is
`since:`, naming the version a behavior appeared in, once there is more than one version to
distinguish.

## Navigation direction is derived, not authored

Cross-document view transitions slide the content, and the slide has to agree with where
the reader went: left for onward, right for back, and vertically for a move between levels.
Nothing in a URL says that — `/reference/tasks` is not "after" `/guides/getting-started` by
any property of the string.

So each page gets an **ordering key** built from the nav tree — `00` for the home page,
`00.01.02` for the second entry of the first section — and the whole map is emitted into
every page as JSON. Zero-padded, dot-joined, it does two jobs with one string: lexicographic
order matches reading order (`.` sorts below every digit, so a parent precedes its children
and a child precedes its parent's next sibling), and a parent's key is a proper prefix of
its children's, so `b.startsWith(a + ".")` distinguishes *below* from merely *after*. Four
directions fall out of one comparison, and reordering a page in the nav moves its slide with
it.

Equal keys get no transition at all. A link to the page you are already on is a scroll to
the top, not a page change, so the click handler cancels the navigation and scrolls for
real. The alternative — capture the content as a named group and let the transition
interpolate its position — was built first and looks wrong: the snapshot slides as one
rigid image, so a band of background opens under the fixed chrome, and the page is reloaded
to animate a picture of itself.

Direction is not the only thing a transition has to know. The topbar, banner, sidebar, TOC
and footer are captured as named elements so they hold still while the content slides — but
a named element the reader *could not see* is the bad case: morphing it from wherever it was
flies it across the screen, so a banner scrolled past a thousand pixels ago drops in from
far above. Each one therefore records whether it was on screen, and the arriving page
compares that against its own: appeared → neutralise the morph and slide it a short way in
from its own edge; disappeared → drop its name here, so the old one simply leaves. Both
sides of that comparison are visibility, not existence, which is what makes one rule cover
a sidebar the home page lacks and a footer that is merely below the fold.

The transitions also constrain how a page *loads*: every page carries
`<link rel="expect" href="#page-end" blocking="render">`, pointing at the footer's id — the
last element on every page — so rendering is held until the document has fully parsed. Slow
delivery otherwise breaks the transition in one of two ways, both reproduced: the incoming
page misses Chrome's paint deadline and the transition is silently dropped (~400 ms of
cold-cache latency sufficed), or worse, it activates on a *partially parsed* document and
the animation runs against a blank body — old page slides out, black slides in, content
pops at the end. `blocking="render"` fixes both at once: the snapshot is never partial, and
the explicit hold extends Chrome's patience instead of exhausting it. CSS inlining and
link prefetching were tried first and rejected — they only narrowed the race (inlining
even lengthens the HTML the parser must stream), while the expect link names the actual
requirement. The cost is progressive rendering, which at these page sizes is nothing; the
footer id is the contract, and renaming it breaks all of this silently. To diagnose a
suspect navigation, log `document.readyState` and `!!event.viewTransition` from `pagereveal`:
`readyState: "loading"` on an incoming transition is the partial-snapshot failure, a missing
`viewTransition` is a skip, and a skip's reason surfaces by catching `viewTransition.ready`.

Neither side of a navigation may reach for the Navigation API to learn where it came from.
`navigation.activation` is Chromium-only and merely touching it throws in Safari, which
kills the handler and leaves the transition undirected — so the previous path goes into
`sessionStorage` on `pagehide`, and the incoming document reads it at parse time rather than
waiting for `pagereveal`. The outgoing document is tagged from a capture-phase click
listener, because the names it assigns are decided while *its* snapshot is captured, before
the new document exists.

The limits are worth stating: two digits per level, so a hundredth sibling would sort wrong,
and the map is inlined per page, which is free at this size and would not be at a thousand.

## Versioning and deployment

Build per tag into a subdirectory: `/v1/`, `/v2/`, latest at the root. No framework here
has versioning built in — Astro, Hugo and Nextra all leave it to the deploy step — so this
is a workflow, not a plugin.

The property that makes old versions cheap is **build once at the tag, never rebuild**.
Archived HTML does not need the toolchain that produced it to still work in three years.

Two decisions with a cheaper-looking alternative:

- **Subdirectories, not subdomains.** `v1.genroc.org` is the nicer URL and the simpler
  mental model — each version is just the whole site, built from a tag, with no
  version-aware routing at all. It is blocked by GitHub Pages allowing **one custom domain
  per repository**, so it would need an archive repo per major version, or a move to
  Netlify/Cloudflare Pages where branch-to-subdomain is native. Reopen if the site moves
  hosts for other reasons.
- **`peaceiris/actions-gh-pages` with `keep_files: true`, never `actions/deploy-pages`.**
  [bench.yml](../.github/workflows/bench.yml) pushes the benchmark time series to
  `gh-pages` under `bench/` with `auto-push`. The artifact-based Pages deployment replaces
  the entire site, which would silently delete that history — and it is stored nowhere
  else.

Assets must be referenced relatively. `hugo.toml` on the blog carries a comment about
root-absolute links requiring a domain root; that pattern breaks the moment a build lands
at `/v1/`.

## Not scoped: a playground

A live validating editor is not planned work. It appears here only to record that the
architecture above does not have to anticipate it.

It does not, because an island is additive by construction: a playground is one React
component on one page, a WASM file served as a static asset, and a web worker — none of
which the other pages know about or pay for. Nothing in this document would be revisited
to add it later.

One fact worth not rediscovering, since it is what makes the idea cheap at all:
**`internal/validation` has no `db`, `engine` or `api` dependency.** Parser, schema subset,
type inference and dataflow analysis are a pure library, so the thing that would run in the
browser is a wrapper over existing code rather than a port of it.

## Still open

- **What explanation `docs/` owes a user, and where it stops.** The guides carry it, but
  the boundary between "why this design" (a spec) and "why you would write it this way" (a
  guide) is drawn by judgement, not by a rule, and the first few guides will set it.
- **Versioning mechanics.** Nothing is built. The per-tag build is a sketch, and the
  version switcher needs a manifest of what exists.
- **How far one TextMate grammar stretches.** Shiki and `editors/vscode/` are meant to
  share a single file. Whether that survives contact with both — Shiki renders, VSCode also
  drives bracket matching and folding — is unverified, and the answer is worth knowing
  before the grammar is written twice.
