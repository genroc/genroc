import { createRequire } from "node:module";

// The extension's own grammars, so the site and the editor colour a definition the same way.
// `injectTo` is how Shiki spells what package.json spells as `injectTo` too; the selector
// inside each file still decides where within the target it applies.
//
// The site loads one grammar the EXTENSION does not: `markers`, which paints `$:` / `${ }`
// wherever they are written. In the editor that question is the server's — it knows which
// slots evaluate and a grammar cannot — but a static page has no server, and every sample
// here is a valid definition, so the lexical answer is the right one and never wrong.
const require = createRequire(import.meta.url);
const load = (name: string) => require(`../../editors/vscode/syntaxes/${name}.tmLanguage.json`);

export const genroc = [
  { ...load("genroc"), name: "genroc" },
  { ...load("genroc-slots"), name: "genroc-slots", injectTo: ["source.genroc"] },
  { ...load("genroc-markers"), name: "genroc-markers", injectTo: ["source.genroc"] },
];
