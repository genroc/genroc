// Chrome that may be absent or scrolled out of frame: morphing it would fly it in from
// off-screen, so it is measured on both sides and <html> is tagged `enter`/`exit` instead.
// Mirrored in ./view-transitions.css, so edit both. `attr` is a dataset key (data-vt-banner).
export type ChromeElement = { attr: string; selector: string }

export const CHROME: ChromeElement[] = [
  { attr: 'vtBanner', selector: '.banner-strip' },
  { attr: 'vtDrawer', selector: '.nav-drawer' },
  { attr: 'vtSidebar', selector: '.shell > .sidebar' },
  { attr: 'vtToc', selector: '.toc' },
  { attr: 'vtFooter', selector: 'footer' },
]
