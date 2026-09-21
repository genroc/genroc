// A page that has children in the nav must show them while the reader is ON it. Collapsing is
// decided per render and baked into the HTML, so the way this breaks is a sidebar that closes
// under the reader's own feet -- every page still builds, and every link still resolves.
//
// It went unnoticed until a folder page carried a body: until then `current` always sat BELOW
// the group, never at it. Checking dist/ rather than nav.ts is what catches that, since the
// state depends on which page the sidebar was rendered for.
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'

const dist = new URL('../dist/', import.meta.url).pathname

function walk(dir) {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name)
    return statSync(path).isDirectory() ? walk(path) : [path]
  })
}

const pages = walk(dist)
  .filter((f) => f.endsWith('.html'))
  .map((f) => ['/' + relative(dist, f).replace(/index\.html$/, ''), readFileSync(f, 'utf8')])
  // A folder page with no body is a refresh stub with no sidebar at all -- nobody reads it,
  // they land on its first child. The site root is the landing page, outside the doc tree.
  .filter(([page, html]) => page !== '/' && !html.includes('http-equiv="refresh"'))

// A page is a nav parent when some other page's URL sits under it. Read off the tree rather
// than off the content collection: the generated pages are what this has to see, and they do
// not exist until the generator has run.
const childrenOf = new Map()
for (const [page] of pages) {
  const parent = page.replace(/[^/]+\/$/, '')
  if (parent === page) continue
  if (pages.some(([p]) => p === parent)) {
    childrenOf.set(parent, (childrenOf.get(parent) ?? []).concat(page))
  }
}

const errors = []
for (const [page, html] of pages) {
  const children = childrenOf.get(page)
  if (!children) continue
  const missing = children.filter((c) => !html.includes(`href="${c}"`))
  if (missing.length) {
    errors.push(`${page} — the sidebar hides ${missing.length} of its ${children.length} children: ${missing.join(', ')}`)
  }
}

if (errors.length) {
  console.error(`nav collapses under the reader (${errors.length}):\n` + errors.map((e) => '  ' + e).join('\n'))
  process.exit(1)
}
console.log(`nav ok — ${childrenOf.size} parent pages`)
