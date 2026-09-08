import { getCollection } from 'astro:content'
import { url } from './url'

export const SECTIONS = [
  { id: 'getting-started', label: 'getting started', blurb: 'install it, learn the shape of it, run one' },
] as const

export type NavEntry = {
  slug: string
  title: string
  description: string
  order: number
  children: NavEntry[]
}

export type NavSection = { id: string; label: string; blurb: string; entries: NavEntry[] }

// One key per page, ordered so that a single string comparison answers "which way".
//
//   home                                 00
//   getting-started/installation         00.01.01
//   getting-started/basic-principles     00.01.02
//   …/basic-principles/checkpoints       00.01.02.01
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

// Nesting is the file path: `a/b/c.mdx` hangs off `a/b.mdx`, and the first segment is the
// section. `order` sorts siblings only. A directory with
// no page of the same name beside it has nothing to hang from, so the build stops rather
// than dropping the page from the nav silently.
export async function navSections(): Promise<NavSection[]> {
  const all = await getCollection('docs')
  const bySlug = new Map<string, NavEntry>(
    all.map((e) => [
      e.id,
      { slug: e.id, title: e.data.title, description: e.data.description, order: e.data.order, children: [] },
    ]),
  )
  const roots = new Map<string, NavEntry[]>(SECTIONS.map((s) => [s.id, []]))

  for (const entry of bySlug.values()) {
    const cut = entry.slug.lastIndexOf('/')
    const parent = cut < 0 ? '' : entry.slug.slice(0, cut)
    const section = roots.get(parent)
    if (section) {
      section.push(entry)
      continue
    }
    const owner = bySlug.get(parent)
    if (!owner) {
      throw new Error(
        `docs: ${entry.slug}.mdx has no parent — create src/content/docs/${parent}.mdx, ` +
          `or move it under a section (${SECTIONS.map((s) => s.id).join(', ')})`,
      )
    }
    owner.children.push(entry)
  }

  const sortDeep = (entries: NavEntry[]) => {
    entries.sort((a, b) => a.order - b.order)
    entries.forEach((e) => sortDeep(e.children))
  }
  return SECTIONS.map((s) => {
    const entries = roots.get(s.id)!
    sortDeep(entries)
    return { ...s, entries }
  })
}

// `docs / reference / tasks` for `reference/tasks/fetch`. The section is a label rather
// than a page, so it is the one crumb with no href.
export async function crumbs(slug: string): Promise<{ label: string; href?: string }[]> {
  const all = await getCollection('docs')
  const titles = new Map(all.map((e) => [e.id, e.data.title]))
  const parts = slug.split('/')
  const section = SECTIONS.find((s) => s.id === parts[0])
  const trail: { label: string; href?: string }[] = [{ label: section ? section.label : parts[0] }]
  for (let i = 1; i < parts.length - 1; i++) {
    const ancestor = parts.slice(0, i + 1).join('/')
    trail.push({ label: titles.get(ancestor) ?? ancestor, href: url(ancestor) })
  }
  return trail
}
