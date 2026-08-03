// Chrome that a transition must treat specially: anything named in CSS that can be absent
// from a page, or scrolled out of frame. Morphing such an element from wherever it was
// flies it in from off-screen, so the script measures each one on both sides of a
// navigation and tags <html> with `enter` / `exit` instead.
//
// These selectors are mirrored by view-transition-name rules in ./view-transitions.css —
// CSS cannot read this list, so adding a piece of chrome means editing both. `attr` is the
// dataset key, so 'vtBanner' writes `data-vt-banner`.
export type ChromeElement = { attr: string; selector: string }

export const CHROME: ChromeElement[] = [
  { attr: 'vtBanner', selector: '.banner-strip' },
  { attr: 'vtDrawer', selector: '.nav-drawer' },
  { attr: 'vtSidebar', selector: '.shell > .sidebar' },
  { attr: 'vtToc', selector: '.toc' },
  { attr: 'vtFooter', selector: 'footer' },
]
