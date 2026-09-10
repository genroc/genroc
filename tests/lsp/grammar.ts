import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { Registry, INITIAL, type IGrammar } from "@shikijs/vscode-textmate";
import { createOnigurumaEngine } from "@shikijs/engine-oniguruma";
import yamlLangs from "@shikijs/langs/yaml";

// Tokenizing a definition the way an editor does: the extension's own grammar files, wired up
// from its own package.json, against a real YAML grammar and a real oniguruma.
//
// The YAML grammar comes from npm rather than an installed VS Code, so this runs on CI — it is
// not byte-identical to the one VS Code ships, but the injection is anchored to `string` and
// `source.genroc`, which both spell the same way.

export const EXTENSION = join(dirname(fileURLToPath(import.meta.url)), "../../editors/vscode");

export interface Contribution {
  language?: string;
  scopeName: string;
  path: string;
  injectTo?: string[];
}

export function contributedGrammars(): Contribution[] {
  const pkg = JSON.parse(readFileSync(join(EXTENSION, "package.json"), "utf8"));
  return pkg.contributes.grammars;
}

export interface Token {
  line: number;
  text: string;
  scopes: string[];
}

const cache = new Map<string, IGrammar>();

/** What the site loads: the extension's grammars plus the marker layer it deliberately omits. */
export function siteGrammars(): Contribution[] {
  return [
    ...contributedGrammars(),
    { scopeName: "source.genroc.markers", path: "./syntaxes/genroc-markers.tmLanguage.json", injectTo: ["source.genroc"] },
  ];
}

export async function loadGenrocGrammar(contributions = contributedGrammars()): Promise<IGrammar> {
  const key = contributions.map((g) => g.scopeName).join(",");
  const hit = cache.get(key);
  if (hit) return hit;
  const byScope = new Map<string, unknown>();
  for (const g of contributions) {
    byScope.set(g.scopeName, JSON.parse(readFileSync(join(EXTENSION, g.path), "utf8")));
  }
  for (const g of yamlLangs) byScope.set(g.scopeName, g);

  const engine = await createOnigurumaEngine(import("shiki/wasm"));
  const registry = new Registry({
    onigLib: {
      createOnigScanner: (sources: string[]) => engine.createScanner(sources),
      createOnigString: (s: string) => engine.createString(s),
    },
    // Read the wiring the way VS Code does, rather than assuming it: an injection is a
    // contribution naming its target in `injectTo`, and the selector inside that grammar
    // decides where within the target it applies. Simulating this is how a package.json that
    // injected nothing at all still passed.
    getInjections: (scope: string) =>
      contributions.filter((g) => (g.injectTo ?? []).includes(scope)).map((g) => g.scopeName),
    loadGrammar: (scope: string) => byScope.get(scope) ?? null,
  } as never);

  const root = contributions.filter((g) => g.language === "genroc" && !g.injectTo);
  if (root.length !== 1) {
    throw new Error(`${root.length} grammars claim the genroc language; VS Code uses one of them`);
  }
  const loaded = await registry.loadGrammar(root[0].scopeName);
  if (!loaded) throw new Error("source.genroc did not load");
  cache.set(key, loaded);
  return loaded;
}

export async function tokenize(code: string, contributions?: Contribution[]): Promise<Token[]> {
  const g = await loadGenrocGrammar(contributions);
  const out: Token[] = [];
  let stack = INITIAL;
  for (const [n, line] of code.split("\n").entries()) {
    const r = g.tokenizeLine(line, stack);
    stack = r.ruleStack;
    for (const t of r.tokens) {
      const text = line.slice(t.startIndex, t.endIndex);
      if (!text.trim()) continue;
      // YAML's own rules hand back a key as its first letter plus the rest; a run of
      // identical scopes is one thing to a reader, and one token to an assertion.
      const last = out.at(-1);
      if (last && last.line === n && sameScopes(last.scopes, t.scopes)) last.text += text;
      else out.push({ line: n, text, scopes: t.scopes });
    }
  }
  return out;
}

function sameScopes(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((s, i) => s === b[i]);
}

/** Each token of `code` mapped to the colour `theme` gives it — what a reader actually sees. */
export async function colourize(code: string, theme: unknown): Promise<Map<string, string>> {
  const { createHighlighterCore } = await import("shiki/core");
  const contributions = contributedGrammars();
  const hi = await createHighlighterCore({
    themes: [theme as never],
    langs: [
      ...contributions.map((g) => ({
        ...JSON.parse(readFileSync(join(EXTENSION, g.path), "utf8")),
        name: g.scopeName,
        ...(g.injectTo ? { injectTo: g.injectTo } : {}),
      })),
      ...yamlLangs,
    ],
    engine: createOnigurumaEngine(import("shiki/wasm")),
  });
  const [tokens] = hi.codeToTokens(code, { lang: "source.genroc", theme: (theme as { name: string }).name }).tokens;
  return new Map(tokens.map((t) => [t.content.trim(), t.color ?? ""]));
}

/**
 * The tokens of the one line of `source` containing `fragment` — how a test names a line, the
 * same rule `at()` uses: a fragment on two lines is refused rather than guessed at.
 */
export function lineOf(tokens: Token[], source: string, fragment: string): Token[] {
  const lines = source.split("\n");
  const hits = lines.flatMap((l, n) => (l.includes(fragment) ? [n] : []));
  if (hits.length !== 1) throw new Error(`${JSON.stringify(fragment)} is on ${hits.length} lines`);
  return tokens.filter((t) => t.line === hits[0]);
}

/** The scopes on the sole token reading `text`. Refuses an ambiguous one, as `at()` does. */
export function scopesOf(tokens: Token[], text: string): string[] {
  return scopesOfToken(tokens, text).scopes;
}

function scopesOfToken(tokens: Token[], text: string): Token {
  const hits = tokens.filter((t) => t.text === text);
  if (hits.length === 0) {
    throw new Error(`no token reads ${JSON.stringify(text)}; got ${tokens.map((t) => t.text).join("|")}`);
  }
  if (hits.length > 1) throw new Error(`${JSON.stringify(text)} is tokenized ${hits.length} times`);
  return hits[0];
}

/** The genroc-specific scope on a token, or undefined where the grammar said nothing about it. */
export function genrocScope(scopes: string[]): string | undefined {
  return scopes.filter((s) => s.endsWith(".genroc") && s !== "source.genroc").pop();
}
