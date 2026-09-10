import { expect, test } from "vitest";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { ORDERS } from "./fixture.ts";
import { EXTENSION, colourize, contributedGrammars, genrocScope, lineOf, scopesOf, siteGrammars, tokenize } from "./grammar.ts";

// What the grammars must say about a definition. Nothing here re-checks YAML — they delegate
// that — only what the language adds to it.
//
// There are two sets, and the split is the point. The EXTENSION injects only what a key makes
// certain (`case:`, `goto:`), because whether any other scalar computes depends on its slot and
// a grammar cannot see slots — internal/lsp/semantic.go answers that. The SITE also loads the
// marker layer, since a static page has no server and its samples are all valid definitions.
const site = siteGrammars();

test("a ${ } interpolation is an expression inside the string it renders into", async () => {
  const url = lineOf(await tokenize(ORDERS, site), ORDERS, "price?customer=");
  expect(genrocScope(scopesOf(url, "${")), "the injection never fired: check the injectionSelector and both scopeNames")
    .toBe("punctuation.definition.template-expression.begin.genroc");
  expect(genrocScope(scopesOf(url, "input"))).toBe("variable.other.genroc");
  expect(genrocScope(scopesOf(url, "customer_id"))).toBe("variable.other.genroc");
  // The literal half of the same string stays literal.
  expect(genrocScope(scopesOf(url, "https://api.example.com/price?customer="))).toBeUndefined();
});

test("a $: leaf types its roots, operators and numbers", async () => {
  const tokens = await tokenize(`out: "$: self.result.total - (input.discount ?? 0)"`, site);
  expect(genrocScope(scopesOf(tokens, "$:"))).toBe("punctuation.definition.template-expression.begin.genroc");
  expect(genrocScope(scopesOf(tokens, "self"))).toBe("variable.other.genroc");
  expect(genrocScope(scopesOf(tokens, "??"))).toBe("keyword.operator.genroc");
  expect(genrocScope(scopesOf(tokens, "0"))).toBe("constant.numeric.genroc");
});

// The region ends before the closing quote on purpose: only the innermost rule's `end` is
// tested, so a region that swallows the quote leaves the string open for the rest of the file.
test("a $: leaf releases the string it is in, and the next line is ordinary YAML", async () => {
  const tokens = await tokenize([`  charged: "$: self.result.total"`, `  method: GET`].join("\n"), site);
  expect(scopesOf(tokens, "method")).toContain("entity.name.tag.yaml");
  expect(genrocScope(scopesOf(tokens, "GET")), "the expression ran past its own scalar")
    .toBeUndefined();
});

test("an escaped quote inside an expression does not end the scalar", async () => {
  const tokens = await tokenize([`  a: "$: outputs.x.code ?? \\"\\""`, `  b: plain`].join("\n"), site);
  expect(genrocScope(scopesOf(tokens, "code"))).toBe("variable.other.genroc");
  expect(scopesOf(tokens, "b")).toContain("entity.name.tag.yaml");
});

// The one expression written bare, so nothing in the string itself says it is one.
test("a case is an expression, read through its own key", async () => {
  const tokens = await tokenize(`      - case: "self.output.charged > 1000"`);
  expect(genrocScope(scopesOf(tokens, "self"))).toBe("variable.other.genroc");
  expect(genrocScope(scopesOf(tokens, ">"))).toBe("keyword.operator.genroc");
  expect(genrocScope(scopesOf(tokens, "1000"))).toBe("constant.numeric.genroc");
  // The key reads as every other key, and both quotes as every other scalar's: the region is
  // meta.embedded, which themes reset to the plain foreground the body wants and they do not.
  expect(scopesOf(tokens, "case")).toContain("entity.name.tag.yaml");
  const quotes = tokens.filter((t) => t.text === '"');
  expect(quotes.length).toBe(2);
  for (const q of quotes) expect(q.scopes).toContain("string.quoted.double.yaml");
});

test("a routing target is a target quoted or bare, and so are end and next", async () => {
  const tokens = await tokenize(
    [`    goto: "$review"`, `    goto: $escalate`, `    switch: end`, `    switch: next`].join("\n"),
  );
  // Sigil included: nothing styles `punctuation.definition.keyword`, so a separately captured
  // `$` renders uncoloured beside a coloured name.
  expect(genrocScope(scopesOf(tokens, "$review"))).toBe("entity.name.function.genroc");
  expect(genrocScope(scopesOf(tokens, "$escalate"))).toBe("entity.name.function.genroc");
  // One slot, one scope: `$name`, `end` and `next` are the closed set a goto may name, and
  // scoping the keywords apart from the names put two colours in the same position.
  const targets = ["$review", "$escalate", "end", "next"].map((t) => genrocScope(scopesOf(tokens, t)));
  expect(new Set(targets).size, `routing targets are scoped ${targets.join("/")}`).toBe(1);
});

// internal/template/template.go: only `$:` at a scalar's start and `${` are markers. Colouring
// anything else that starts with `$` would claim the file does something it does not.
test("a $ that is not a marker is literal text", async () => {
  const tokens = await tokenize([`  a: "costs $$5 and $notavar"`, `  # \${ input.x } in a comment`].join("\n"), site);
  expect(genrocScope(scopesOf(tokens, "$$"))).toBe("constant.character.escape.genroc");
  expect(genrocScope(scopesOf(tokens, "5 and $notavar"))).toBeUndefined();
  expect(genrocScope(scopesOf(tokens, " ${ input.x } in a comment"))).toBeUndefined();
});

// A ${ } ends at the first `}` the grammar is not already inside — an object literal has to be
// matched as a balanced pair or a lambda's body closes the interpolation early.
test("an object literal inside an expression does not end it", async () => {
  const tokens = await tokenize(`  over: "$: map(input.rows, i => {id: i.id})"`, site);
  expect(genrocScope(scopesOf(tokens, "map"))).toBe("support.function.builtin.genroc");
  expect(genrocScope(scopesOf(tokens, "=>"))).toBe("keyword.operator.arrow.genroc");
  expect(genrocScope(scopesOf(tokens, ")"))).toBe("punctuation.separator.genroc");
});

// The injection is offered over the whole document, not only inside strings, because a bare
// `goto: $x` and a `case:` are not reachable from a string scope. That reach is what could
// derail YAML's own parsing, and an illegal token is how it would show.
test("every shipped definition tokenizes with nothing illegal in it", async () => {
  const roots = ["../examples", "../cmd/genctl/templates"];
  const files: string[] = [];
  const walk = (dir: string) => {
    for (const e of readdirSync(dir, { withFileTypes: true })) {
      if (e.isDirectory()) walk(join(dir, e.name));
      else if (e.name.endsWith(".genroc.yaml")) files.push(join(dir, e.name));
    }
  };
  for (const r of roots) walk(r);
  expect(files.length, "no definitions found to check").toBeGreaterThan(5);

  for (const f of files) {
    const bad = (await tokenize(readFileSync(f, "utf8")))
      .filter((t) => t.scopes.some((s) => s.startsWith("invalid.")))
      .map((t) => `${f}:${t.line + 1} ${JSON.stringify(t.text)}`);
    expect(bad, "the injection broke YAML's own parsing of this file").toEqual([]);
  }
});

// VS Code loads a grammar only when the contributed scopeName equals the one the file declares,
// and on a mismatch it loads neither. The Go test asserts the same thing for the base grammar;
// this one covers the injection, whose selector must also name the scope it is injected into.
// The editor must NOT carry the marker layer: it paints `$:` wherever it is written, and
// `id: "$: tick"` is plain text. That was reported from the editor and is why the server owns
// the question now. A regression here silently reinstates the false positive.
test("the extension does not inject the marker layer", async () => {
  const editor = await tokenize(`  - id: "$: tick"`);
  for (const t of editor) {
    expect(genrocScope(t.scopes), `the editor painted ${JSON.stringify(t.text)} in a literal slot`).toBeUndefined();
  }
  // The site does paint it, or this test would pass by loading nothing.
  const onSite = await tokenize(`  - id: "$: tick"`, site);
  expect(onSite.some((t) => genrocScope(t.scopes)), "the marker layer painted nothing at all").toBe(true);
});

test("the injection is contributed under the scope name it declares", () => {
  const grammars = contributedGrammars();
  const injections = grammars.filter((g) => g.injectTo);
  expect(injections.length, "nothing is injected, so a definition is highlighted as plain YAML").toBe(1);
  for (const g of injections) {
    const declared = JSON.parse(readFileSync(join(EXTENSION, g.path), "utf8"));
    expect(declared.scopeName).toBe(g.scopeName);
    expect(g.injectTo).toContain("source.genroc");
    // The selector lives in the grammar file; package.json has no such field and ignores it.
    expect(declared.injectionSelector).toContain("source.genroc");
  }
});

// `language` names the grammar FOR a language. A second contribution claiming it replaces the
// first, so VS Code tokenized definitions with the injection alone — no `include: source.yaml`,
// and every YAML key, value and quote in the file lost its colour. Reported from the editor.
test("only the base grammar claims the genroc language", () => {
  const grammars = contributedGrammars();
  const claiming = grammars.filter((g) => g.language === "genroc");
  expect(claiming.map((g) => g.scopeName), "an injection must not carry `language`").toEqual(["source.genroc"]);
  const base = JSON.parse(readFileSync(join(EXTENSION, claiming[0].path), "utf8"));
  expect(JSON.stringify(base.patterns), "the base grammar is what pulls YAML in").toContain("source.yaml");
});

// A member path is one referent, so every segment carries one scope. Scoping the root apart
// from the rest — variable.language against variable.other.property — is the conventional
// split, and it paints `self.previous.count` in two colours in any theme that follows it
// (#569CD6 then #9CDCFE in Dark+). Reported twice: from the docs site, then from the editor.
test("a member path carries one scope from root to leaf", async () => {
  const tokens = await tokenize(`      tally: "$: (self.previous.count ?? 0) + 1"`, site);
  const path = ["self", "previous", "count"].map((seg) => genrocScope(scopesOf(tokens, seg)));
  expect(new Set(path).size, `self.previous.count is scoped ${path.join("/")}`).toBe(1);
});

test("the docs theme colours a whole member path or none of it", async () => {
  const { light, dark } = await import("../../docs/src/shiki-theme.ts");
  const line = `      tally: "$: (self.previous.count ?? 0) + 1"`;

  for (const theme of [light, dark]) {
    const coloured = await colourize(line, theme);
    const path = ["self", "previous", "count"].map((seg) => coloured.get(seg));
    expect(new Set(path).size, `${theme.name} paints self.previous.count as ${path.join("/")}`).toBe(1);
  }
});
