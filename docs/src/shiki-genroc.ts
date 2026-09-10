import { createRequire } from "node:module";

// The extension's own grammars, so the site and the editor colour a definition the same way.
// `injectTo` is how Shiki spells what package.json spells as `injectTo` too; the selector
// inside each file still decides where within the target it applies.
//
// `markers` is injected into `source.genroc` here and into markdown in the extension, and the
// difference is the root: Shiki tokenizes a fence AS the language, so an injection naming a
// markdown scope would never fire. Same reason either way — no server reaches a static page or
// a fence, and every sample in one is a valid definition, so the lexical answer is never wrong.
const require = createRequire(import.meta.url);
const load = (name: string) => require(`../../editors/vscode/syntaxes/${name}.tmLanguage.json`);

export const genroc = [
  { ...load("genroc"), name: "genroc" },
  { ...load("genroc-slots"), name: "genroc-slots", injectTo: ["source.genroc"] },
  { ...load("genroc-markers"), name: "genroc-markers", injectTo: ["source.genroc"] },
];
