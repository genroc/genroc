import { mkdirSync, writeFileSync } from "fs";
import { join } from "path";
import { beforeAll, afterAll, expect, test } from "vitest";
import { at, CompletionItem, Doc, Lsp, orders, useWorkspace } from "./helpers.ts";

// The path a `$<resolver>:` directive names: offered while it is typed, and followed once it is
// written. Resolution itself never treats the argument as a path — it is handed to the resolver
// verbatim — so this is the editor guessing, shell-style, and the guess is the whole rule: an
// argument beginning `/` or `.` is a path and nothing else is.

let lsp: Lsp;
let dir: string;

beforeAll(async () => {
  useWorkspace();
  dir = orders.uri.slice("file://".length).replace(/\/[^/]+$/, "");
  // A resolver registry, so the ext filter has something to read. `bundle` takes .ts only;
  // `open` declares no suffixes, which accepts anything.
  writeFileSync(
    join(dir, ".genroc"),
    [
      "resolvers:",
      "  - name: bundle",
      "    phase: code",
      "    ext: ['.ts']",
      "    command: ['true']",
      "  - name: open",
      "    phase: code",
      "    command: ['true']",
      "",
    ].join("\n"),
    "utf8",
  );
  writeFileSync(join(dir, "worker.ts"), "export const x = 1\n", "utf8");
  writeFileSync(join(dir, "notes.md"), "# notes\n", "utf8");
  // A dotfile that DOES pass the suffix filter, so the dot rule is tested on its own rather
  // than through the filter that would have hidden it anyway.
  writeFileSync(join(dir, ".hidden.genroc.yaml"), "name: hidden\ntasks: []\n", "utf8");
  mkdirSync(join(dir, "sub"), { recursive: true });
  writeFileSync(join(dir, "sub", "nested.genroc.yaml"), "name: nested\ntasks: []\n", "utf8");
  lsp = await Lsp.start();
}, 60_000);
afterAll(async () => lsp?.stop());

let n = 0;
/** A definition whose child action carries the directive line given. */
function doc(directive: string): Doc {
  const name = `paths_${n++}`;
  return {
    uri: `file://${dir}/${name}.genroc.yaml`,
    text: [
      `name: ${name}`,
      "tasks:",
      "  - id: call",
      "    action:",
      "      type: child",
      directive,
      "    switch: [{ goto: end }]",
      "output: { ok: true }",
      "",
    ].join("\n"),
  };
}

test("`./` offers the definitions beside the file, filtered to what $process accepts", async () => {
  const d = doc('      <<: "$process: ./"');
  const labels = await lsp.completions(at('      <<: "$process: ./<|>"', d));
  // Every *.genroc.yaml in the directory, and the folder that may hold more.
  expect(labels).toContain("orders.genroc.yaml");
  expect(labels).toContain("shipment.genroc.yaml");
  expect(labels).toContain("sub/");
  // `$process` declares its suffixes, so nothing else is a candidate.
  expect(labels).not.toContain("worker.ts");
  expect(labels).not.toContain("notes.md");
});

test("a resolver's own ext list is what filters, not $process's", async () => {
  const d = doc('      result_schema: "$bundle: ./"');
  const labels = await lsp.completions(at('      result_schema: "$bundle: ./<|>"', d));
  expect(labels).toContain("worker.ts");
  expect(labels).not.toContain("orders.genroc.yaml");
  expect(labels).not.toContain("notes.md");
});

test("a resolver declaring no ext accepts anything, so everything is offered", async () => {
  const d = doc('      result_schema: "$open: ./"');
  const labels = await lsp.completions(at('      result_schema: "$open: ./<|>"', d));
  expect(labels).toContain("worker.ts");
  expect(labels).toContain("notes.md");
  expect(labels).toContain("orders.genroc.yaml");
});

test("a name no resolver carries is left unfiltered rather than answered with nothing", async () => {
  const d = doc('      result_schema: "$nosuch: ./"');
  const labels = await lsp.completions(at('      result_schema: "$nosuch: ./<|>"', d));
  expect(labels).toContain("notes.md");
});

test("a folder leads somewhere, so it carries its slash and sorts first", async () => {
  const d = doc('      <<: "$process: ./"');
  const items = await lsp.completionDetails(at('      <<: "$process: ./<|>"', d));
  expect(items["sub/"]).toBeDefined();
  expect(items["sub/"].detail).toBe("folder");
});

test("descending a folder offers what is inside it", async () => {
  const d = doc('      <<: "$process: ./sub/"');
  const labels = await lsp.completions(at('      <<: "$process: ./sub/<|>"', d));
  expect(labels).toEqual(["nested.genroc.yaml"]);
});

test("a partial name narrows nothing server-side — the editor filters, the range is what matters", async () => {
  const d = doc('      <<: "$process: ./ship"');
  // The item replaces from the start of `ship`, so choosing one does not leave `./shipship…`.
  const labels = await lsp.completions(at('      <<: "$process: ./<^ship>"', d));
  expect(labels).toContain("shipment.genroc.yaml");
});

// Where this started: `$process: ` answered with nothing, so the first keystroke of a path had
// to be guessed blind. An empty argument has no meaning yet to invent, so it is offered one.
test("an argument with nothing typed yet offers the directory beside the file", async () => {
  const d = doc('      <<: "$process: "');
  const labels = await lsp.completions(at('      <<: "$process: <|>"', d));
  expect(labels).toContain("shipment.genroc.yaml");
  expect(labels, "a folder is how a reader reaches the rest of the tree").toContain("sub/");
});

/** What choosing an item actually writes into the document. */
function inserted(items: CompletionItem[], label: string): string | undefined {
  return items.find((i) => i.label === label)?.textEdit?.newText;
}

// A relative path is written `./name`: explicit is clearer, and a bare name is the one spelling
// a resolver may read as something that is not a path at all.
test("choosing from an empty argument writes a ./ path, not a bare name", async () => {
  const items = await lsp.completionItems(at('      <<: "$process: <|>"', doc('      <<: "$process: "')));
  expect(inserted(items, "shipment.genroc.yaml")).toBe("./shipment.genroc.yaml");
  expect(inserted(items, "sub/")).toBe("./sub/");
});

test("a path that already carries its ./ does not gain a second one", async () => {
  const items = await lsp.completionItems(at('      <<: "$process: ./<|>"', doc('      <<: "$process: ./"')));
  expect(inserted(items, "shipment.genroc.yaml")).toBe("shipment.genroc.yaml");
  expect(inserted(items, "sub/")).toBe("sub/");
});

test("an absolute path is left absolute", async () => {
  const d = doc(`      <<: "$process: ${dir}/"`);
  const items = await lsp.completionItems(at(`      <<: "$process: ${dir}/<|>"`, d));
  expect(inserted(items, "shipment.genroc.yaml")).toBe("shipment.genroc.yaml");
});

// `.` is a trigger character, because a member list needs one — so accepting a lone dot put a
// directory listing on screen the instant anyone typed one, dotfiles and all. One more
// keystroke says which of `./` and `../` was meant, and there is nothing to guess until then.
test("a lone dot offers nothing — it is a prefix of two spellings, not either of them", async () => {
  const d = doc('      <<: "$process: .x"');
  const labels = await lsp.completions(at('      <<: "$process: <^.x>"', d));
  expect(labels).not.toContain("shipment.genroc.yaml");
  expect(labels).not.toContain("sub/");
});

test("a bare dotted name is not a path either", async () => {
  const d = doc('      <<: "$process: .hidden"');
  const labels = await lsp.completions(at('      <<: "$process: <^.hidden>"', d));
  expect(labels).not.toContain("shipment.genroc.yaml");
});

test("the next keystroke opens it: `./` lists, where `.` did not", async () => {
  const labels = await lsp.completions(
    at('      <<: "$process: ./<|>"', doc('      <<: "$process: ./"')),
  );
  expect(labels).toContain("shipment.genroc.yaml");
});

test("`../` reaches the parent, and back down again", async () => {
  const base = dir.slice(dir.lastIndexOf("/") + 1);
  const d = doc(`      <<: "$process: ../${base}/"`);
  const labels = await lsp.completions(at(`      <<: "$process: ../${base}/<|>"`, d));
  expect(labels).toContain("shipment.genroc.yaml");
});

test("an argument that is not path-like is left alone", async () => {
  const d = doc('      <<: "$process: lodash"');
  const labels = await lsp.completions(at('      <<: "$process: <^lodash>"', d));
  // A resolver's argument may be anything; offering files here would invent a meaning.
  expect(labels).not.toContain("orders.genroc.yaml");
});

test("dotfiles stay hidden until one is asked for", async () => {
  const bare = await lsp.completions(at('      <<: "$process: ./<|>"', doc('      <<: "$process: ./"')));
  expect(bare).toContain("shipment.genroc.yaml");
  expect(bare.some((l) => l.startsWith("."))).toBe(false);

  // Two characters, because `<^…>` lands the cursor in the MIDDLE of what it names — with one
  // the cursor would sit before the dot, and the dot would not have been typed yet.
  const asked = await lsp.completions(
    at('      <<: "$process: ./<^.h>"', doc('      <<: "$process: ./.h"')),
  );
  // It passes the suffix filter, so only the leading dot was ever hiding it — which is what
  // makes this the dot rule and not the filter tested twice.
  expect(asked).toContain(".hidden.genroc.yaml");
});

test("a directory that does not exist yet answers with nothing, not with the wrong thing", async () => {
  const d = doc('      <<: "$process: ./nowhere/"');
  const labels = await lsp.completions(at('      <<: "$process: ./nowhere/<|>"', d));
  // Not the action's remaining KEYS, which is what falling through would offer.
  expect(labels).toEqual([]);
});

test("the path is clickable: go-to-definition opens the file it names", async () => {
  const d = doc('      <<: "$process: ./shipment.genroc.yaml"');
  expect(await lsp.definition(at('      <<: "$process: ./<^shipment>.genroc.yaml"', d))).toBe(
    "shipment.genroc.yaml:1",
  );
});

test("a path into a folder is clickable too", async () => {
  const d = doc('      <<: "$process: ./sub/nested.genroc.yaml"');
  expect(await lsp.definition(at('      <<: "$process: ./sub/<^nested>.genroc.yaml"', d))).toBe(
    "nested.genroc.yaml:1",
  );
});

test("a path naming no file is not clickable", async () => {
  const d = doc('      <<: "$process: ./gone.genroc.yaml"');
  expect(await lsp.definition(at('      <<: "$process: ./<^gone>.genroc.yaml"', d))).toBe("");
});

test("a non-path argument is not clickable", async () => {
  const d = doc('      <<: "$process: lodash"');
  expect(await lsp.definition(at('      <<: "$process: <^lodash>"', d))).toBe("");
});
