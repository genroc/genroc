import { readdirSync, readFileSync, writeFileSync } from "fs";
import { join } from "path";
import { load } from "js-yaml";

// Names are literal rather than generated so the expected block reads as real output.

const ROOT = new URL("../cli/testdata/compat/", import.meta.url).pathname;

export interface CompatCase {
  /** Directory + file stem, e.g. "children/child-output-widened". */
  id: string;
  file: string;
  /** Applied in order, each as one `genctl apply -f … --channel …` batch. */
  apply: { channel: string; definitions: Record<string, unknown>[] }[];
  /** Definitions submitted as `-f` instead of applied: what an apply WOULD take. */
  submit?: Record<string, unknown>[];
  /** Arguments after `genctl compat`. */
  run: string[];
  /** The rendered report this case expects. Must be the last key in the file. */
  expect: string;
}

export function loadGroup(group: string): CompatCase[] {
  const dir = join(ROOT, group);
  const cases = readdirSync(dir)
    .filter((f) => f.endsWith(".yaml"))
    .sort()
    .map((f) => {
      const file = join(dir, f);
      const doc = load(readFileSync(file, "utf8")) as Omit<CompatCase, "id" | "file">;
      const id = `${group}/${f.replace(/\.yaml$/, "")}`;
      if (!doc?.apply?.length) throw new Error(`${id}: no apply steps`);
      if (!doc?.run?.length) throw new Error(`${id}: no run arguments`);
      return { ...doc, id, file };
    });
  return cases;
}

/** On the shared server a name collision does not error: one case compares the other's definitions. */
export function assertUniqueNames(groups: string[]): void {
  const owner = new Map<string, string>();
  const claim = (kind: string, name: string, id: string) => {
    const key = `${kind}:${name}`;
    const held = owner.get(key);
    if (held && held !== id) {
      throw new Error(`${kind} name "${name}" is claimed by both ${held} and ${id}; names are global`);
    }
    owner.set(key, id);
  };
  for (const group of groups) {
    for (const c of loadGroup(group)) {
      for (const step of c.apply) {
        claim("channel", step.channel, c.id);
        for (const def of step.definitions) claim("process", def.name as string, c.id);
      }
      for (const def of c.submit ?? []) claim("process", def.name as string, c.id);
    }
  }
}

/** Replaces everything from the last `expect: |` on, so it must be the file's last key. */
export function writeExpected(c: CompatCase, output: string): void {
  const text = readFileSync(c.file, "utf8");
  const marker = text.lastIndexOf("\nexpect: |");
  const head = marker === -1 ? text.trimEnd() + "\n" : text.slice(0, marker + 1);
  const body = output
    .trimEnd()
    .split("\n")
    .map((line) => (line ? `  ${line}` : ""))
    .join("\n");
  writeFileSync(c.file, `${head}expect: |\n${body}\n`);
}
