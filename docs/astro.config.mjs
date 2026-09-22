// @ts-check
import { defineConfig } from "astro/config";
import mdx from "@astrojs/mdx";
import rehypeSlug from "rehype-slug";
import rehypeAutolinkHeadings from "rehype-autolink-headings";
import relativeMarkdownLinks from "astro-rehype-relative-markdown-links";
import { light, dark } from "./src/shiki-theme.ts";
import { genroc } from "./src/shiki-genroc.ts";
import { existsSync, readFileSync } from "node:fs";
import hoverData, { fenceKey } from "./scripts/hover-data.mjs";

// The live site serves from the apex of genroc.org (public/CNAME), so `base` is `/`;
// the deploy workflow overrides DOCS_BASE only for an archived build at a versioned
// subpath. Every asset and link must therefore be resolved through `url()` in
// src/lib/url.ts rather than written root-absolute. Images in content are the exception:
// `~/assets/...` (the tsconfig paths alias) reaches Astro's image pipeline, which applies
// `base` itself -- and errors on a path that resolves to nothing, where a root-absolute 404s.
const base = process.env.DOCS_BASE ?? "/";

// Hovers for the `genroc` fences the language server can type -- generated in
// astro:config:setup, which runs after this module is evaluated, so they are read on first use
// rather than imported. A fence with none is left exactly as it was.
const fencesFile = new URL("./src/samples/fences.hovers.json", import.meta.url);
let fenceHovers;
const spansFor = (code) => {
  fenceHovers ??= existsSync(fencesFile) ? JSON.parse(readFileSync(fencesFile, "utf8")) : {};
  return fenceHovers[fenceKey(code)];
};

// The server writes markdown, and uses two things a tooltip should show differently: a code
// span for a path or a type, bold for the slot a description belongs to.
const tipChildren = (markdown) =>
  markdown
    .split(/(`[^`]+`|\*\*[^*]+\*\*)/)
    .filter(Boolean)
    .map((part) => {
      const code = part.startsWith("`");
      const bold = part.startsWith("**");
      if (!code && !bold) return { type: "text", value: part };
      return {
        type: "element",
        tagName: code ? "span" : "strong",
        properties: code ? { className: ["code"] } : {},
        children: [{ type: "text", value: part.slice(code ? 1 : 2, code ? -1 : -2) }],
      };
    });

// A tip opens upwards unless the block has no room up there -- it is clipped by the block's
// own scroll box either way, so the side is decided per span: how many lines the text wraps to
// at --tip-wrap characters, against the lines above and below it.
const TIP_WRAP = 64;
const opensDown = (markdown, line, lines) => {
  const rows = Math.ceil(markdown.replace(/`/g, "").length / TIP_WRAP) + 0.5;
  return line - 1 < rows && lines - line >= line - 1;
};

// Shiki's decorations do the hard half: they wrap a range of a line, splitting whatever tokens
// it crosses. The tip itself is added afterwards, since a decoration can carry attributes but
// not children.
const genrocHovers = {
  name: "genroc-hovers",
  preprocess(code, options) {
    const spans = spansFor(code);
    if (!spans) return;
    const lines = code.trimEnd().split("\n").length;
    options.decorations = spans.map((s) => ({
      start: { line: s.line - 1, character: s.from },
      end: { line: s.line - 1, character: s.to },
      properties: {
        class: opensDown(s.markdown, s.line, lines) ? "hovered opens-down" : "hovered",
        style: `--col:${s.from};--line:${s.line}`,
        "data-tip": s.markdown,
      },
    }));
  },
  root(hast) {
    const walk = (node) => {
      const tip = node.properties?.dataTip ?? node.properties?.["data-tip"];
      if (tip) {
        delete node.properties.dataTip;
        delete node.properties["data-tip"];
        node.children.push({
          type: "element",
          tagName: "span",
          properties: {
            className: String(node.properties.class ?? node.properties.className).includes("opens-down")
              ? ["tip", "below"]
              : ["tip"],
          },
          children: tipChildren(tip),
        });
      }
      node.children?.forEach(walk);
    };
    walk(hast);
  },
};

export default defineConfig({
  site: "https://genroc.org",
  base,
  trailingSlash: "always",
  // `prefetchAll` is what opts every internal link in; without it the strategy below applies
  // to nothing, since it only governs links carrying a bare `data-astro-prefetch`.
  // Leave `experimental.clientPrerender` off: it upgrades this to speculation-rules
  // *prerender*, which runs the incoming page's scripts before the click, and
  // view-transitions/ViewTransitions.astro reads the outgoing page's state at that point.
  prefetch: { prefetchAll: true, defaultStrategy: "hover" },
  integrations: [mdx(), hoverData()],
  markdown: {
    rehypePlugins: [
      // Links between pages are written as paths to the source file -- `./error-handling.mdx`,
      // what an editor completes and follows -- and this turns them into the URL that page is
      // served from. `collectionBase: false` because the docs collection is the site root: a
      // slug is the file path, with no `/docs` segment in front (src/content.config.ts).
      [
        relativeMarkdownLinks,
        { collectionBase: false, base, trailingSlash: "always" },
      ],
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
      // `genroc` is YAML plus the expression injection; `yaml` has to be named too, since
      // the grammar reaches it by scope name rather than bundling it.
      langs: [...genroc, "yaml"],
      transformers: [genrocHovers],
      defaultColor: false,
      wrap: false,
    },
  },
});
