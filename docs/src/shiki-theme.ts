import type { ThemeRegistration } from 'shiki'

// Hand-written in the site's palette. A rule's colour key is `foreground`: `color` is silently
// ignored.
const scopes = (c: Record<string, string>) => [
  { scope: ['comment', 'punctuation.definition.comment'], settings: { foreground: c.muted, fontStyle: 'italic' } },
  { scope: ['entity.name.tag', 'support.type.property-name', 'meta.object-literal.key'], settings: { foreground: c.key } },
  { scope: ['string', 'string.quoted', 'meta.embedded.line'], settings: { foreground: c.string } },
  { scope: ['constant.numeric', 'constant.language', 'constant.language.boolean'], settings: { foreground: c.constant } },
  { scope: ['keyword', 'storage', 'keyword.operator'], settings: { foreground: c.accent } },
  { scope: ['variable', 'variable.other', 'support.function'], settings: { foreground: c.fg } },
  { scope: ['punctuation', 'meta.brace'], settings: { foreground: c.muted } },
  // Markers and routing keywords carry the accent. Member paths do not: `self` and the segments
  // after it have different scopes, and tinting one would highlight only part of the path.
  { scope: ['punctuation.definition.template-expression', 'keyword.control.flow'], settings: { foreground: c.accent } },
  { scope: ['entity.name.function', 'support.function.builtin'], settings: { foreground: c.accent } },
  { scope: ['markup.inserted', 'meta.diff.header.to-file'], settings: { foreground: c.added } },
  { scope: ['markup.deleted', 'meta.diff.header.from-file'], settings: { foreground: c.removed } },
]

export const theme: ThemeRegistration = {
  name: 'genroc',
  type: 'dark',
  colors: { 'editor.background': '#1d1d1d', 'editor.foreground': '#d6d6d2' },
  settings: scopes({
    fg: '#d6d6d2',
    muted: '#8a8a84',
    accent: '#e07a4f',
    key: '#d6d6d2',
    string: '#8fb996',
    constant: '#d4a76a',
    added: '#8fb996',
    removed: '#e07a4f',
  }),
}
