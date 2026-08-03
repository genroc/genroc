import { getCollection } from 'astro:content'
import { url } from './url'

export const SECTIONS = [
  { id: 'guides', label: 'guides', blurb: 'task-oriented, with the why' },
  { id: 'reference', label: 'reference', blurb: 'the contract, no argument' },
] as const

export type NavEntry = { slug: string; title: string; description: string; order: number }

// One key per page, ordered so that a single string comparison answers "which way".
//
//   home                 00
//   guides/…             00.01.01, 00.01.02
//   reference/…          00.02.01
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
  sections.forEach((section, si) => {
    section.entries.forEach((entry, ei) => {
      tree[normPath(url(entry.slug))] = `${ROOT_KEY}.${pad(si + 1)}.${pad(ei + 1)}`
    })
  })
  return tree
}

export async function navSections(): Promise<{ id: string; label: string; blurb: string; entries: NavEntry[] }[]> {
  const all = await getCollection('docs')
  return SECTIONS.map((s) => ({
    ...s,
    entries: all
      .filter((e) => e.data.section === s.id)
      .map((e) => ({ slug: e.id, title: e.data.title, description: e.data.description, order: e.data.order }))
      .sort((a, b) => a.order - b.order),
  }))
}
