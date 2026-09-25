// Every internal href in dist/ must resolve to a page that exists, and every #fragment to an
// id that page actually renders. Checking the built HTML rather than the sources is what makes
// relative links checkable at all: `../error-handling/#panic` only has a meaning once you know
// which URL the page carrying it was served from.
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'

const dist = new URL('../dist/', import.meta.url).pathname
const base = (process.env.DOCS_BASE ?? '/').replace(/\/$/, '')

function walk(dir) {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name)
    return statSync(path).isDirectory() ? walk(path) : [path]
  })
}

const files = walk(dist)
const pages = new Map() // site path -> ids it renders
const assets = new Set(files.map((f) => '/' + relative(dist, f)))

for (const file of files.filter((f) => f.endsWith('.html'))) {
  const html = readFileSync(file, 'utf8')
  const ids = new Set([...html.matchAll(/\sid="([^"]+)"/g)].map((m) => m[1]))
  pages.set('/' + relative(dist, file).replace(/index\.html$/, ''), ids)
}

// Swagger UI renders its anchors client-side, so a deep link is checked against the spec it
// loads: `#/<tag>/<operationId>`, keyed on the operation's first tag.
const swaggerOps = new Set()
for (const item of Object.values(JSON.parse(readFileSync(join(dist, 'openapi.json'), 'utf8')).paths)) {
  for (const op of Object.values(item)) {
    if (op?.operationId && op.tags?.length) swaggerOps.add(`/${op.tags[0]}/${op.operationId}`)
  }
}

const errors = []

// Links are authored as paths to a source file, but reach dist as the URL the rehype plugin
// rewrote them to -- so report the .mdx the reader has to open, not just the page it became.
function source(page) {
  const slug = page.replace(/^\/|\/$/g, '') || 'index'
  const path = `src/content/docs/${slug}.mdx`
  return existsSync(new URL(`../${path}`, import.meta.url).pathname) ? path : page
}

for (const [page, ids] of pages) {
  const html = readFileSync(join(dist, page, 'index.html'), 'utf8')

  for (const [, href] of html.matchAll(/\shref="([^"]+)"/g)) {
    if (/^(https?:|mailto:|data:|#$)/.test(href)) continue

    const { pathname, hash } = new URL(href, `http://l${page}`)
    // dist holds the site without its base prefix, so an href written through url() carries one
    // the tree does not have.
    const target = base && pathname.startsWith(base + '/') ? pathname.slice(base.length) : pathname
    const fragment = decodeURIComponent(hash.slice(1))

    if (target === '/swagger/index.html' && fragment) {
      if (!swaggerOps.has(fragment)) errors.push(`${source(page)}: ${href} — no such operation in openapi.json`)
      continue
    }
    if (!pages.has(target)) {
      if (!assets.has(target)) errors.push(`${source(page)}: ${href} — no such page`)
      continue
    }
    const targetIds = target === page ? ids : pages.get(target)
    if (fragment && !targetIds.has(fragment)) {
      errors.push(`${source(page)}: ${href} — page exists, #${fragment} does not`)
    }
  }
}

if (errors.length) {
  console.error(`broken links (${errors.length}):\n` + errors.map((e) => '  ' + e).join('\n'))
  process.exit(1)
}
console.log(`links ok — ${pages.size} pages`)
