import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { buildGenctlBinary } from "../helpers/cli.ts";
import { ORDERS, SHIPMENT } from "./fixture.ts";

// Driving `genctl lsp` the way an editor does: a real process, real framing, real binary.
//
// A test names a position by quoting the line it is on and marking it, so the assertion shows
// the code it is about instead of a line number that drifts the moment the fixture is edited.
//
//   <|>          the cursor is here
//   <|text>      the cursor is here and `text` has NOT been typed yet — it is removed from the
//                buffer, which is how a half-written expression is spelled
//   <^text>      the cursor is inside `text`, which stays; for hover and go-to-definition
//
// The quoted fragment must appear exactly once in the document, so a test can never silently
// come to mean a different line.

export interface Cursor {
  uri: string;
  text: string;
  line: number;
  character: number;
  /** The fragment as quoted, for failure messages. */
  quoted: string;
}

const MARKER = /<(\||\^)([^>]*)>/;

export function at(snippet: string, doc: Doc = orders): Cursor {
  const m = MARKER.exec(snippet);
  if (!m) throw new Error(`no <|> or <^…> marker in: ${snippet}`);
  if (MARKER.test(snippet.slice(m.index + m[0].length))) {
    throw new Error(`more than one marker in: ${snippet}`);
  }

  const [whole, mode, inner] = m;
  // The fragment as it appears in the document: markers removed, their text kept.
  const needle = snippet.slice(0, m.index) + inner + snippet.slice(m.index + whole.length);
  const found = doc.text.indexOf(needle);
  if (found < 0) throw new Error(`not in ${doc.uri}:\n  ${needle}`);
  if (doc.text.indexOf(needle, found + 1) >= 0) {
    throw new Error(`appears more than once, so the cursor is ambiguous:\n  ${needle}`);
  }

  const markerAt = found + m.index;
  let text = doc.text;
  let offset = markerAt;
  if (mode === "|") {
    // Not yet typed: take it back out, and leave the cursor where it would be mid-word.
    text = doc.text.slice(0, markerAt) + doc.text.slice(markerAt + inner.length);
  } else {
    // Hovering: land in the middle, so a cursor at a token boundary is never the question.
    offset = markerAt + Math.floor(inner.length / 2);
  }

  const before = text.slice(0, offset);
  const line = before.split("\n").length - 1;
  return {
    uri: doc.uri,
    text,
    line,
    character: offset - (before.lastIndexOf("\n") + 1),
    quoted: snippet.trim(),
  };
}

/**
 * A copy of `doc` with each search string replaced. Diagnostics need a document that is wrong,
 * and a marker places a cursor rather than editing a word — so a mistake is written here, where
 * the test can show both halves of it. Each search string must appear exactly once.
 */
export function edit(doc: Doc, changes: Record<string, string>): Doc {
  let text = doc.text;
  for (const [from, to] of Object.entries(changes)) {
    const first = text.indexOf(from);
    if (first < 0) throw new Error(`not in ${doc.uri}:\n  ${from}`);
    if (text.indexOf(from, first + 1) >= 0) {
      throw new Error(`appears more than once, so the edit is ambiguous:\n  ${from}`);
    }
    text = text.slice(0, first) + to + text.slice(first + from.length);
  }
  return { uri: doc.uri, text };
}

export interface Diagnostic {
  range: { start: { line: number; character: number } };
  message: string;
  code?: string;
}

export interface Doc {
  uri: string;
  text: string;
}

let workspace = "";
export let orders: Doc;
export let shipment: Doc;

/** Writes the fixture to a temp workspace, so cross-file navigation has files to find. */
export function useWorkspace(): void {
  workspace = mkdtempSync(join(tmpdir(), "genroc-lsp-"));
  writeFileSync(join(workspace, "orders.genroc.yaml"), ORDERS);
  writeFileSync(join(workspace, "shipment.genroc.yaml"), SHIPMENT);
  orders = { uri: `file://${join(workspace, "orders.genroc.yaml")}`, text: ORDERS };
  shipment = { uri: `file://${join(workspace, "shipment.genroc.yaml")}`, text: SHIPMENT };
}

export class Lsp {
  private child: ChildProcessWithoutNullStreams;
  private buffer = Buffer.alloc(0);
  private pending = new Map<number, (result: unknown) => void>();
  private awaiting = new Map<string, (ds: Diagnostic[]) => void>();
  private nextId = 1;
  private opened = new Set<string>();
  private version = 1;

  private constructor(bin: string) {
    this.child = spawn(bin, ["lsp"], { stdio: ["pipe", "pipe", "pipe"] });
    this.child.stdout.on("data", (chunk: Buffer) => this.consume(chunk));
  }

  static async start(): Promise<Lsp> {
    const lsp = new Lsp(buildGenctlBinary());
    await lsp.request("initialize", { rootUri: `file://${workspace}` });
    lsp.notify("initialized", {});
    return lsp;
  }

  async stop(): Promise<void> {
    await this.request("shutdown", null);
    this.notify("exit", null);
    this.child.kill();
  }

  /** Completion labels at the cursor, sorted — the set, not the order the server found it in. */
  async completions(cursor: Cursor): Promise<string[]> {
    const items = (await this.ask(cursor, "textDocument/completion")) as { label: string }[];
    return items.map((i) => i.label).sort();
  }

  /** Completion labels mapped to what the editor shows beside them, and the prose behind it. */
  async completionDetails(
    cursor: Cursor,
  ): Promise<Record<string, { detail: string; documentation: string }>> {
    const items = (await this.ask(cursor, "textDocument/completion")) as {
      label: string;
      detail?: string;
      documentation?: string;
    }[];
    return Object.fromEntries(
      items.map((i) => [i.label, { detail: i.detail ?? "", documentation: i.documentation ?? "" }]),
    );
  }

  /** The hover markdown, or "" where the server has nothing to say. */
  async hover(cursor: Cursor): Promise<string> {
    const r = (await this.ask(cursor, "textDocument/hover")) as {
      contents?: { value?: string };
    } | null;
    return r?.contents?.value ?? "";
  }

  /** Every diagnostic for a document, as `<line>: <message>` (1-based). */
  async diagnostics(doc: Doc): Promise<string[]> {
    const cursor: Cursor = { ...doc, line: 0, character: 0, quoted: "" };
    const waited = new Promise<Diagnostic[]>((resolve) => this.awaiting.set(doc.uri, resolve));
    this.sync(cursor);
    const ds = await waited;
    return ds.map((d) => `${d.range.start.line + 1}: ${d.message}`);
  }

  /** Diagnostics for a buffer a marker has edited. */
  async diagnosticsAt(cursor: Cursor): Promise<string[]> {
    return this.diagnostics({ uri: cursor.uri, text: cursor.text });
  }

  /** Where a reference points, as `<file>:<line>` (1-based), or "" for nowhere. */
  async definition(cursor: Cursor): Promise<string> {
    const r = (await this.ask(cursor, "textDocument/definition")) as {
      uri: string;
      range: { start: { line: number } };
    } | null;
    if (!r) return "";
    return `${r.uri.split("/").pop()}:${r.range.start.line + 1}`;
  }

  private async ask(cursor: Cursor, method: string): Promise<unknown> {
    this.sync(cursor);
    return this.request(method, {
      textDocument: { uri: cursor.uri },
      position: { line: cursor.line, character: cursor.character },
    });
  }

  /** Full sync: every request re-sends the whole buffer, since a marker may have edited it. */
  private sync(cursor: Cursor): void {
    if (!this.opened.has(cursor.uri)) {
      this.opened.add(cursor.uri);
      this.notify("textDocument/didOpen", {
        textDocument: { uri: cursor.uri, version: ++this.version, text: cursor.text },
      });
      return;
    }
    this.notify("textDocument/didChange", {
      textDocument: { uri: cursor.uri, version: ++this.version },
      contentChanges: [{ text: cursor.text }],
    });
  }

  private request(method: string, params: unknown): Promise<unknown> {
    const id = this.nextId++;
    const done = new Promise<unknown>((resolve) => this.pending.set(id, resolve));
    this.send({ jsonrpc: "2.0", id, method, params });
    return done;
  }

  private notify(method: string, params: unknown): void {
    this.send({ jsonrpc: "2.0", method, params });
  }

  private send(msg: unknown): void {
    const body = Buffer.from(JSON.stringify(msg), "utf8");
    this.child.stdin.write(`Content-Length: ${body.length}\r\n\r\n`);
    this.child.stdin.write(body);
  }

  private consume(chunk: Buffer): void {
    this.buffer = Buffer.concat([this.buffer, chunk]);
    for (;;) {
      const sep = this.buffer.indexOf("\r\n\r\n");
      if (sep < 0) return;
      const header = this.buffer.subarray(0, sep).toString();
      const length = Number(/Content-Length: (\d+)/.exec(header)?.[1]);
      if (!Number.isFinite(length) || this.buffer.length < sep + 4 + length) return;
      const body = this.buffer.subarray(sep + 4, sep + 4 + length).toString("utf8");
      this.buffer = this.buffer.subarray(sep + 4 + length);
      const msg = JSON.parse(body) as {
        id?: number;
        result?: unknown;
        method?: string;
        params?: { uri: string; diagnostics: Diagnostic[] };
      };
      if (msg.method === "textDocument/publishDiagnostics" && msg.params) {
        const waiter = this.awaiting.get(msg.params.uri);
        if (waiter) {
          this.awaiting.delete(msg.params.uri);
          waiter(msg.params.diagnostics);
        }
        continue;
      }
      if (msg.id !== undefined && this.pending.has(msg.id)) {
        this.pending.get(msg.id)!(msg.result ?? null);
        this.pending.delete(msg.id);
      }
    }
  }
}
