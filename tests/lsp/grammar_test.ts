import { expect, test } from "vitest";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { ORDERS } from "./fixture.ts";
import { EXTENSION, contributedGrammars, genrocScope, lineOf, scopesOf, siteGrammars, tokenize, tokenizeIn } from "./grammar.ts";

// The EXTENSION injects only what a key makes certain (`case:`, `goto:`): any other scalar depends
// on its slot, which internal/lsp/semantic.go answers. The SITE, having no server, adds the markers.
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
  const tokens = await tokenize([`  charged: "$: self.result.total"`, `  method: get`].join("\n"), site);
  expect(scopesOf(tokens, "method")).toContain("entity.name.tag.yaml");
  expect(genrocScope(scopesOf(tokens, "get")), "the expression ran past its own scalar")
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
  // One slot, one scope: `$name`, `end` and `next` are one closed set.
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

// The injection spans the whole document (a bare `goto: $x` is in no string scope), so it could
// derail YAML's own parsing.
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

// The marker layer paints `$:` wherever it is written, but `id: "$: tick"` is plain text.
test("the extension does not inject the marker layer", async () => {
  const editor = await tokenize(`  - id: "$: tick"`);
  for (const t of editor) {
    expect(genrocScope(t.scopes), `the editor painted ${JSON.stringify(t.text)} in a literal slot`).toBeUndefined();
  }
  // The site does paint it, or this test would pass by loading nothing.
  const onSite = await tokenize(`  - id: "$: tick"`, site);
  expect(onSite.some((t) => genrocScope(t.scopes)), "the marker layer painted nothing at all").toBe(true);
});

// On a scopeName mismatch VS Code loads neither grammar; the Go test covers the base grammar.
test("the injection is contributed under the scope name it declares", () => {
  const grammars = contributedGrammars();
  const injections = grammars.filter((g) => g.injectTo);
  expect(injections.length, "nothing is injected, so a definition is highlighted as plain YAML").toBeGreaterThan(0);
  for (const g of injections) {
    const declared = JSON.parse(readFileSync(join(EXTENSION, g.path), "utf8"));
    expect(declared.scopeName).toBe(g.scopeName);
    // The selector lives in the grammar file; package.json has no such field and ignores it.
    expect(declared.injectionSelector, `${g.scopeName} is injected but selects on nothing`).toBeTruthy();
  }
});

// Injections are collected for the ROOT scope only, and .mdx is a language of its own to VS Code.
test("the slot and marker layers reach a fence in both markdown and MDX", () => {
  const grammars = contributedGrammars();
  const into = (scope: string) =>
    grammars.filter((g) => (g.injectTo ?? []).includes(scope)).map((g) => g.scopeName);
  expect(into("source.genroc"), "a definition is highlighted as plain YAML").toContain("source.genroc.slots");
  expect(into("source.mdx"), "an .mdx page gets what a .md page does, or neither").toEqual(into("text.html.markdown"));
  expect(into("text.html.markdown"), "a fence needs the block, its slots and its markers").toHaveLength(3);
});

// The map and the grammar are in different files and neither reads the other. Without the map a
// fence still colours, but VS Code keeps markdown's brackets, comments and indentation inside it.
test("the fence's content scope is the one package.json maps to the language", () => {
  const block = contributedGrammars().find((g) => g.scopeName === "markdown.genroc.codeblock");
  expect(Object.entries(block!.embeddedLanguages ?? {})).toEqual([["meta.embedded.block.genroc", "genroc"]]);
  const declared = JSON.parse(readFileSync(join(EXTENSION, block!.path), "utf8"));
  const scopes = declared.repository.fence.patterns[0].contentName.split(" ");
  expect(scopes, "the language map keys off this").toContain("meta.embedded.block.genroc");
  // An `include` does not push the included grammar's own scopeName, so the two injections find
  // nothing on the stack unless the fence names it here.
  expect(scopes, "the slot and marker injections select on this").toContain("source.genroc");
});

// A fence is the docs site's case inside an editor: no server reaches it, and a sample in one is
// a valid definition, so the marker layer is the right answer there and only there.
const FENCED = [
  "Prose **before**.",
  "",
  "```genroc",
  "name: ticker",
  "tasks:",
  "  - id: tick",
  '    goto: "$review"',
  '    url: "https://x?c=${ input.customer_id }"',
  '    tally: "$: self.previous ?? 0"',
  "```",
  "",
  "Prose after.",
].join("\n");

test.each([["markdown", "text.html.markdown"], ["MDX", "source.mdx"]])(
  "a ```genroc fence in %s is highlighted as a definition",
  async (_name, root) => {
    const tokens = await tokenizeIn(root, FENCED);
    expect(scopesOf(tokens, "ticker"), "the fence never opened: check the language name in `begin`")
      .toContain("meta.embedded.block.genroc");
    expect(scopesOf(tokens, "name")).toContain("entity.name.tag.yaml");
    expect(genrocScope(scopesOf(tokens, "$review")), "the slot layer is not injected into this root")
      .toBe("entity.name.function.genroc");
    expect(genrocScope(scopesOf(tokens, "customer_id")), "the marker layer is not injected into this root")
      .toBe("variable.other.genroc");
    expect(genrocScope(scopesOf(tokens, "$:"))).toBe("punctuation.definition.template-expression.begin.genroc");

    // And it closes. A fence that runs on takes the rest of the page with it.
    const after = lineOf(tokens, FENCED, "Prose after");
    expect(after.flatMap((t) => t.scopes).filter((s) => s.includes("genroc")), "the fence ran past its own close")
      .toEqual([]);
  },
);

// A second contribution claiming `language` replaces the base, and with it `include: source.yaml`.
test("only the base grammar claims the genroc language", () => {
  const grammars = contributedGrammars();
  const claiming = grammars.filter((g) => g.language === "genroc");
  expect(claiming.map((g) => g.scopeName), "an injection must not carry `language`").toEqual(["source.genroc"]);
  const base = JSON.parse(readFileSync(join(EXTENSION, claiming[0].path), "utf8"));
  expect(JSON.stringify(base.patterns), "the base grammar is what pulls YAML in").toContain("source.yaml");
});

// Not the conventional variable.language root: that paints one path in two colours.
test("a member path carries one scope from root to leaf", async () => {
  const tokens = await tokenize(`      tally: "$: (self.previous.count ?? 0) + 1"`, site);
  const path = ["self", "previous", "count"].map((seg) => genrocScope(scopesOf(tokens, seg)));
  expect(new Set(path).size, `self.previous.count is scoped ${path.join("/")}`).toBe(1);
});
