import type { ThemeRegistration } from 'shiki'

// Two hand-written TextMate themes rather than a stock Shiki one: the palette is the
// site's, and `themes: {light, dark}` emits both colours per token so the theme toggle
// needs no JavaScript. Keep the scope lists identical in both — a scope present in only
// one theme renders unstyled in the other.
// A rule's colour key is `foreground` — TextMate's spelling. `color` is silently ignored,
// leaving every token the editor foreground.
const scopes = (c: Record<string, string>) => [
  { scope: ['comment', 'punctuation.definition.comment'], settings: { foreground: c.muted, fontStyle: 'italic' } },
  { scope: ['entity.name.tag', 'support.type.property-name', 'meta.object-literal.key'], settings: { foreground: c.key } },
  { scope: ['string', 'string.quoted', 'meta.embedded.line'], settings: { foreground: c.string } },
  { scope: ['constant.numeric', 'constant.language', 'constant.language.boolean'], settings: { foreground: c.constant } },
  { scope: ['keyword', 'storage', 'keyword.operator'], settings: { foreground: c.accent } },
  { scope: ['variable', 'variable.other', 'support.function'], settings: { foreground: c.fg } },
  { scope: ['punctuation', 'meta.brace'], settings: { foreground: c.muted } },
  { scope: ['entity.name.function', 'support.function.builtin'], settings: { foreground: c.accent } },
  { scope: ['markup.inserted', 'meta.diff.header.to-file'], settings: { foreground: c.added } },
  { scope: ['markup.deleted', 'meta.diff.header.from-file'], settings: { foreground: c.removed } },
]

export const light: ThemeRegistration = {
  name: 'genroc-light',
  type: 'light',
  colors: { 'editor.background': '#ececea', 'editor.foreground': '#1c1c1c' },
  settings: scopes({
    fg: '#1c1c1c',
    muted: '#6b6b6b',
    accent: '#b5532b',
    key: '#1c1c1c',
    string: '#3f6b4f',
    constant: '#8a5a1e',
    added: '#3f6b4f',
    removed: '#b5532b',
  }),
}

export const dark: ThemeRegistration = {
  name: 'genroc-dark',
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
