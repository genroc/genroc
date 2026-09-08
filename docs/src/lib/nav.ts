import { getCollection } from 'astro:content'
import { url } from './url'

export type NavEntry = {
  slug: string
  title: string
  description?: string
  order: number
  children: NavEntry[]
  /** Where a click goes. A folder page has no body, so it is its first child's URL. */
  href: string
}

export type NavSection = { id: string; label: string; href: string; entries: NavEntry[] }

// One key per page, ordered so that a single string comparison answers "which way".
//
//   home                                       00
//   guides/getting-started                     00.01.01
//   guides/getting-started/installation        00.01.01.01
//   guides/process-definition                  00.01.02
//
// Two properties do the work. Lexicographic order matches reading order, because "."
// sorts below every digit — so a parent precedes its children and a child precedes its
// parent's next sibling. And a parent's key is a proper prefix of its children's, so
// `b.startsWith(a + ".")` means b is *below* a rather than merely after it.
//
// Two digits per level: an entry past the 99th would sort wrong. Levels come from the
// nav, not the URL, so reordering a page changes its key and the slide follows.
const KEY_DIGITS = 2
const ROOT_KEY = '00'
const pad = (n: number) => String(n).padStart(KEY_DIGITS, '0')

// Trailing slashes and fragments are noise for comparison; the client-side half of this
// (in Base.astro) must normalize the same way or every lookup misses.
export function normPath(path: string): string {
  return path.replace(/#.*$/, '').replace(/\/+$/, '') || '/'
}

export async function navTree(): Promise<Record<string, string>> {
  const sections = await navSections()
  const tree: Record<string, string> = { [normPath(url('/'))]: ROOT_KEY }
  const walk = (entries: NavEntry[], prefix: string) => {
    entries.forEach((entry, i) => {
      const key = `${prefix}.${pad(i + 1)}`
      tree[normPath(url(entry.slug))] = key
      walk(entry.children, key)
    })
  }
  sections.forEach((section, si) => walk(section.entries, `${ROOT_KEY}.${pad(si + 1)}`))
  return tree
}

// The whole structure is the file tree: `a/b/c.mdx` hangs off `a/b.mdx`, and a page at the
// root is a SECTION. `order` sorts siblings only, so each folder page places itself among
// its own kind and nothing compares an order across two parents.
//
// A folder page carries no body: it exists to give the folder a name and a position, and a
// click on it lands on its first child ([...slug].astro redirects anyone who reaches the URL
// itself). That is why `description` is optional — there is nothing to describe.
export async function navSections(): Promise<NavSection[]> {
  const all = await getCollection('docs')
  const bySlug = new Map<string, NavEntry>(
    all.map((e) => [
      e.id,
      {
        slug: e.id,
        title: e.data.title,
        description: e.data.description,
        order: e.data.order,
        children: [],
        href: url(e.id),
        // Kept off NavEntry: whether a page has content decides where its link goes, and
        // nothing downstream needs to ask again.
        empty: !e.body?.trim(),
      } as NavEntry & { empty: boolean },
    ]),
  )

  const sections: (NavEntry & { empty?: boolean })[] = []
  for (const entry of bySlug.values()) {
    const cut = entry.slug.lastIndexOf('/')
    if (cut < 0) {
      sections.push(entry)
      continue
    }
    const owner = bySlug.get(entry.slug.slice(0, cut))
    if (!owner) {
      throw new Error(
        `docs: ${entry.slug}.mdx has no parent — create src/content/docs/${entry.slug.slice(0, cut)}.mdx`,
      )
    }
    owner.children.push(entry)
  }

  // Depth-first, deepest first: a folder's href is its first child's, and that child may be a
  // folder too, so the child must be resolved before the parent reads it.
  const settle = (entries: (NavEntry & { empty?: boolean })[]) => {
    entries.sort((a, b) => a.order - b.order)
    for (const e of entries) {
      settle(e.children)
      if (e.empty && e.children.length > 0) e.href = e.children[0].href
    }
  }
  settle(sections)

  return sections.map((s) => ({ id: s.slug, label: s.title, href: s.href, entries: s.children }))
}

// `docs / Guides / Getting started` for `guides/getting-started/installation`. Every crumb but
// the last is a folder page, which redirects — so they are all clickable and all land somewhere.
export async function crumbs(slug: string): Promise<{ label: string; href?: string }[]> {
  const all = await getCollection('docs')
  const titles = new Map(all.map((e) => [e.id, e.data.title]))
  const parts = slug.split('/')
  const trail: { label: string; href?: string }[] = []
  for (let i = 0; i < parts.length - 1; i++) {
    const ancestor = parts.slice(0, i + 1).join('/')
    trail.push({ label: titles.get(ancestor) ?? ancestor, href: url(ancestor) })
  }
  return trail
}

/** Where a slug's link lands: its own page, or its first child's when it has no body. */
export async function hrefFor(slug: string): Promise<string | undefined> {
  const find = (entries: NavEntry[]): NavEntry | undefined => {
    for (const e of entries) {
      if (e.slug === slug) return e
      const hit = find(e.children)
      if (hit) return hit
    }
  }
  for (const s of await navSections()) {
    if (s.id === slug) return s.href
    const hit = find(s.entries)
    if (hit) return hit.href
  }
}
