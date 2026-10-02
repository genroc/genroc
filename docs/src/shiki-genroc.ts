import { createRequire } from "node:module";

// The VS Code extension's grammars, so site and editor colour alike. `markers` injects into
// `source.genroc` here but into markdown in the extension: Shiki tokenizes a fence AS the
// language, so a markdown-scope injection would never fire.
const require = createRequire(import.meta.url);
const load = (name: string) => require(`../../editors/vscode/syntaxes/${name}.tmLanguage.json`);

// `genroc-lsp` is an alias, not a second grammar: the label marks a block the docs build asks
// the language server about (docs/scripts/hover-data.mjs), and it must colour like any other.
export const genroc = [
  { ...load("genroc"), name: "genroc", aliases: ["genroc-lsp"] },
  { ...load("genroc-slots"), name: "genroc-slots", injectTo: ["source.genroc"] },
  { ...load("genroc-markers"), name: "genroc-markers", injectTo: ["source.genroc"] },
];
