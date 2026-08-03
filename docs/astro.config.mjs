// @ts-check
import { defineConfig } from "astro/config";
import mdx from "@astrojs/mdx";
import rehypeSlug from "rehype-slug";
import rehypeAutolinkHeadings from "rehype-autolink-headings";
import { light, dark } from "./src/shiki-theme.ts";

// The live site serves from the apex of genroc.org (public/CNAME), so `base` is `/`;
// the deploy workflow overrides DOCS_BASE only for an archived build at a versioned
// subpath. Every asset and link must therefore be resolved through `url()` in
// src/lib/url.ts rather than written root-absolute.
export default defineConfig({
  site: "https://genroc.org",
  base: process.env.DOCS_BASE ?? "/",
  trailingSlash: "always",
  // The whole site is a handful of small HTML documents, so `load` — every internal link
  // on the page, not just hovered ones — costs little and makes the first click instant on
  // touch too. Leave `experimental.clientPrerender` off: it upgrades this to speculation-rules
  // *prerender*, which runs the incoming page's scripts before the click, and
  // view-transitions/ViewTransitions.astro reads the outgoing page's state at that point.
  prefetch: { defaultStrategy: "hover" },
  integrations: [mdx()],
  markdown: {
    rehypePlugins: [
      // Astro assigns heading ids in a plugin that runs after these, so autolink would
      // see no id to point at. rehype-slug puts one there first.
      rehypeSlug,
      [
        rehypeAutolinkHeadings,
        {
          behavior: "append",
          properties: {
            class: "heading-anchor",
            ariaHidden: "true",
            tabIndex: -1,
          },
          // An empty element, with the '#' drawn by CSS: a text node here would land in
          // the heading before Astro collects headings, putting a stray '#' in the TOC.
          content: {
            type: "element",
            tagName: "span",
            properties: {},
            children: [],
          },
        },
      ],
    ],
    shikiConfig: {
      themes: { light, dark },
      defaultColor: false,
      wrap: false,
    },
  },
});
