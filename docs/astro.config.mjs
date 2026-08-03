// @ts-check
import { defineConfig } from 'astro/config'
import mdx from '@astrojs/mdx'
import rehypeSlug from 'rehype-slug'
import rehypeAutolinkHeadings from 'rehype-autolink-headings'
import { light, dark } from './src/shiki-theme.ts'

// `base` is set by the deploy workflow for an archived build (`/v1/`); every asset and
// link on the site must therefore be resolved through `url()` in src/lib/url.ts rather
// than written root-absolute.
export default defineConfig({
  site: 'https://genroc.github.io',
  base: process.env.DOCS_BASE ?? '/',
  trailingSlash: 'always',
  integrations: [mdx()],
  markdown: {
    rehypePlugins: [
      // Astro assigns heading ids in a plugin that runs after these, so autolink would
      // see no id to point at. rehype-slug puts one there first.
      rehypeSlug,
      [
        rehypeAutolinkHeadings,
        {
          behavior: 'append',
          properties: { class: 'heading-anchor', ariaHidden: 'true', tabIndex: -1 },
          // An empty element, with the '#' drawn by CSS: a text node here would land in
          // the heading before Astro collects headings, putting a stray '#' in the TOC.
          content: { type: 'element', tagName: 'span', properties: {}, children: [] },
        },
      ],
    ],
    shikiConfig: {
      themes: { light, dark },
      defaultColor: false,
      wrap: false,
    },
  },
})
