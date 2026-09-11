import { getCollection } from 'astro:content'
import { url } from './url'

export type NavEntry = {
  slug: string
  title: string
  description?: string
  order: number
  /** Resolved, never undefined: an unset page takes its parent's, and the root's is false. */
  autoCollapse: boolean
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
        autoCollapse: e.data.autoCollapse as boolean,
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
  // folder too, so the child must be resolved before the parent reads it. `autoCollapse` runs
  // the other way -- it is inherited, so a level resolves its own before descending.
  const settle = (entries: (NavEntry & { empty?: boolean })[], inherited: boolean) => {
    entries.sort((a, b) => a.order - b.order)
    for (const e of entries) {
      e.autoCollapse = e.autoCollapse ?? inherited
      settle(e.children, e.autoCollapse)
      if (e.empty && e.children.length > 0) e.href = e.children[0].href
    }
  }
  settle(sections, false)

  return sections.map((s) => ({ id: s.slug, label: s.title, href: s.href, entries: s.children }))
}

/** Every page by slug, with the URL its links must use — never a folder's own, which redirects. */
async function linkTargets(): Promise<Map<string, { title: string; href: string }>> {
  const out = new Map<string, { title: string; href: string }>()
  const walk = (entries: NavEntry[]) => {
    for (const e of entries) {
      out.set(e.slug, { title: e.title, href: e.href })
      walk(e.children)
    }
  }
  for (const s of await navSections()) {
    out.set(s.id, { title: s.label, href: s.href })
    walk(s.entries)
  }
  return out
}

// `docs / Guides / Getting started` for `guides/getting-started/installation`. Every crumb is a
// folder page, so each links where that folder resolves, never to the folder URL -- a click
// that redirects strands the view transition on a document it computed no direction for.
export async function crumbs(slug: string): Promise<{ label: string; href?: string }[]> {
  const targets = await linkTargets()
  const parts = slug.split('/')
  const trail: { label: string; href?: string }[] = []
  for (let i = 0; i < parts.length - 1; i++) {
    const ancestor = parts.slice(0, i + 1).join('/')
    const target = targets.get(ancestor)
    trail.push({ label: target?.title ?? ancestor, href: target?.href })
  }
  return trail
}

/** Where a slug's link lands: its own page, or its first child's when it has no body. */
export async function hrefFor(slug: string): Promise<string | undefined> {
  return (await linkTargets()).get(slug)?.href
}

/** Reading order across every section, depth first. Folder pages are skipped: they redirect,
    so landing on one from a "next" link would bounce the reader somewhere else again. */
export async function readingOrder(): Promise<NavEntry[]> {
  const out: NavEntry[] = []
  const walk = (entries: NavEntry[]) => {
    for (const e of entries) {
      if (e.href === url(e.slug)) out.push(e)
      walk(e.children)
    }
  }
  for (const s of await navSections()) walk(s.entries)
  return out
}

export async function neighbours(slug: string): Promise<{ prev?: NavEntry; next?: NavEntry }> {
  const order = await readingOrder()
  const i = order.findIndex((e) => e.slug === slug)
  if (i < 0) return {}
  return { prev: order[i - 1], next: order[i + 1] }
}
