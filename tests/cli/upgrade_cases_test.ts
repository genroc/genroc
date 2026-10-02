import { afterEach, beforeAll, describe, test } from "vitest";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { load } from "js-yaml";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { startGenroc, tmpPath, type GenrocProcess } from "../helpers/server.ts";

/**
 * `genctl upgrade` against a running instance, one case per file in testdata/upgrade/<group>/.
 * Asserts the migrated state, not the command's output: the rendering is compat's business.
 */

const GROUPS = ["happy", "shapes", "refused", "tree"];
const DIR = join(import.meta.dirname, "testdata", "upgrade");

interface UpgradeCase {
  id: string;
  path: string;
  /** Definition sets applied in order; each becomes the next version. */
  apply: { definitions: object[] }[];
  /** The instance to create and how far to drive it before upgrading. */
  start: {
    process: string;
    input?: Record<string, unknown>;
    ticks?: number;
    /** Clock milliseconds each tick advances, for a case that has to let a timer fire. */
    advance_ms?: number;
  };
  /** The state the instance must rest in when the upgrade runs, so ticks that miss it fail. */
  resting?: RestingState;
  /** The state after the upgrade; `state_keys` catches lost bookkeeping (external_input, _spawn_*). */
  after?: RestingState;
  /** Arguments after `genctl upgrade`. */
  run: string[];
  /** The move must be refused (non-zero exit); `after` pins that the instance did not move. */
  refused?: { /** A fragment the refusal must name, so it points at the real reason. */ says?: string };
  /** Run the case once per position, upgrading after `ticks`; fields left out fall back to the case's. */
  at?: Array<{
    ticks: number;
    resting?: RestingState;
    after?: RestingState;
    finish?: UpgradeCase["finish"];
    /** `false` opts this position out of a case-level refusal: the allowed side of a boundary. */
    refused?: UpgradeCase["refused"] | false;
  }>;
  /** Drive the upgraded instance on: catches a migration that looks right but breaks the next tick. */
  finish?: {
    status: string;
    /** The process output, compared whole: a migration that dropped a slot shows as a hole. */
    output?: Record<string, unknown>;
    /** Ticks to allow before giving up. */
    ticks?: number;
    advance_ms?: number;
  };
}

interface RestingState {
  task?: string;
  status?: string;
  /** Dotted context paths to values, JSON-compared: an expected `null` fails on a missing key. */
  values?: Record<string, unknown>;
  /** The version the row is on — the one thing a refused move must not have changed. */
  version?: number;
  phase?: string;
  outputs?: string[];
  /** Top-level keys of the stored state, sorted. */
  state_keys?: string[];
}

const STATE_FIELDS = new Set(["task", "status", "values", "version", "phase", "outputs", "state_keys"]);

function loadGroup(group: string): UpgradeCase[] {
  const dir = join(DIR, group);
  return readdirSync(dir)
    .filter((f) => f.endsWith(".yaml"))
    .sort()
    .map((f) => {
      const path = join(dir, f);
      const doc = load(readFileSync(path, "utf8")) as Omit<UpgradeCase, "id" | "path">;
      return { ...doc, id: `${group}/${f.replace(/\.yaml$/, "")}`, path };
    });
}

let genctlBin: string;
let server: GenrocProcess | undefined;

beforeAll(async () => {
  genctlBin = buildGenctlBinary();
}, 90_000);

afterEach(async () => {
  await server?.stop();
  server = undefined;
});

async function runCase(c: UpgradeCase, at?: NonNullable<UpgradeCase["at"]>[number]): Promise<void> {
  const ticks = at ? at.ticks : c.start.ticks ?? 0;
  const resting = at?.resting ?? c.resting;
  const after = { ...c.after, ...at?.after };
  const finish = at?.finish ?? c.finish;
  const refused = at && "refused" in at ? at.refused || undefined : c.refused;
  // Manual-tick: a case names exact tick counts, so the server must take no step on its own.
  server = await startGenroc({ db: tmpPath("upgrade_case", ".db"), poll: 0, maxConcurrent: 4 });
  const env = { GENROC_SERVER: server.baseUrl };

  const applied = runCli(genctlBin, ["apply", "-f", writeDefs(c.apply[0].definitions)], env);
  if (!applied.ok) throw new Error(`apply v1 failed for ${c.id}: ${applied.stderr}`);

  const started = await server.client.POST("/instances", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: { process: c.start.process, input: c.start.input ?? {} } as any,
  });
  if (started.error) throw new Error(`start failed for ${c.id}: ${JSON.stringify(started.error)}`);

  for (let i = 0; i < ticks; i++) {
    await server.client.POST("/tick", { body: { advance_ms: c.start.advance_ms ?? 0 } });
  }

  const instanceID = started.data!.id;

  /** The stored state, with detail's separated fields folded back in. */
  function wholeState(got: Record<string, unknown>): Record<string, unknown> {
    const out = { ...((got.state ?? {}) as Record<string, unknown>) };
    for (const [field, slot] of [
      ["output", "output"],
      ["error_data", "_error_data"],
      ["external_input", "external_input"],
    ] as const) {
      if (got[field] !== undefined) out[slot] = got[field];
    }
    return out;
  }

  async function assertState(label: string, want: RestingState) {
    const unread = Object.keys(want).filter((k) => !STATE_FIELDS.has(k));
    if (unread.length > 0) {
      throw new Error(`${c.id} (${label}): unknown state field(s) ${unread.join(", ")} -- nothing reads them, so they assert nothing`);
    }
    const { data } = await server!.client.GET("/instances/{id}/detail", {
      params: { path: { id: instanceID } },
    });
    const got = data as unknown as Record<string, unknown>;
    const ctx = wholeState(got);
    const outs = (ctx.outputs ?? {}) as Record<string, unknown>;
    const actual = {
      task: got.task,
      status: got.status,
      version: got.version,
      phase: got.phase ?? "",
      outputs: Object.keys(outs).sort(),
      state_keys: Object.keys(ctx).sort(),
    };
    const mismatch = (field: string, a: unknown, w: unknown) =>
      new Error(`${c.id} (${label}): ${field} is ${JSON.stringify(a)}, not ${JSON.stringify(w)}\n  state: ${JSON.stringify(actual)}`);

    if (want.task !== undefined && actual.task !== want.task) throw mismatch("task", actual.task, want.task);
    if (want.version !== undefined && actual.version !== want.version) {
      throw mismatch("version", actual.version, want.version);
    }
    if (want.status !== undefined && actual.status !== want.status) throw mismatch("status", actual.status, want.status);
    if (want.phase !== undefined && actual.phase !== want.phase) {
      throw mismatch("phase", actual.phase, want.phase);
    }
    for (const [path, expected] of Object.entries(want.values ?? {})) {
      let at: unknown = ctx;
      for (const seg of path.split(".")) {
        at = at === null || at === undefined ? undefined : (at as Record<string, unknown>)[seg];
      }
      if (JSON.stringify(at) !== JSON.stringify(expected)) {
        throw mismatch(`context.${path}`, at === undefined ? "<missing>" : at, expected);
      }
    }
    for (const [field, w] of [["outputs", want.outputs], ["state_keys", want.state_keys]] as const) {
      if (w === undefined) continue;
      const sorted = [...w].sort();
      const a = actual[field];
      if (JSON.stringify(a) !== JSON.stringify(sorted)) throw mismatch(field, a, sorted);
    }
  }

  if (resting) await assertState(`after ${ticks} tick(s)`, resting);

  for (const step of c.apply.slice(1)) {
    const next = runCli(genctlBin, ["apply", "-f", writeDefs(step.definitions)], env);
    if (!next.ok) throw new Error(`apply failed for ${c.id}: ${next.stderr}`);
  }

  const res = runCli(genctlBin, ["upgrade", ...c.run], env);
  if (refused) {
    if (res.ok) {
      throw new Error(`${c.id}: the move was expected to be refused, but exited 0\n${res.stdout}`);
    }
    const said = res.stdout + res.stderr;
    if (refused.says && !said.includes(refused.says)) {
      throw new Error(`${c.id}: refused, but not for the stated reason — wanted ${JSON.stringify(refused.says)} in:\n${said}`);
    }
  } else if (!res.ok) {
    throw new Error(`${c.id}: upgrade failed (exit ${res.exitCode})\n${res.stdout}${res.stderr}`);
  }
  if (Object.keys(after).length > 0) await assertState("after the upgrade", after);

  if (!finish) return;
  const want = finish;
  const budget = want.ticks ?? 20;
  let got: Record<string, unknown> = {};
  for (let i = 0; i <= budget; i++) {
    const { data } = await server.client.GET("/instances/{id}/detail", {
      params: { path: { id: instanceID } },
    });
    got = data as unknown as Record<string, unknown>;
    if (["completed", "failed", "raised"].includes(got.status as string)) break;
    if (i === budget) {
      throw new Error(
        `${c.id}: still ${got.status} at task ${JSON.stringify(got.task)} ${budget} ticks past the upgrade`,
      );
    }
    await server.client.POST("/tick", { body: { advance_ms: want.advance_ms ?? c.start.advance_ms ?? 0 } });
  }
  if (got.status !== want.status) {
    throw new Error(
      `${c.id} (running on): status is ${JSON.stringify(got.status)}, not ${JSON.stringify(want.status)}` +
        `\n  error: ${JSON.stringify(got.error_message ?? got.error_code)}`,
    );
  }
  if (want.output !== undefined) {
    const actual = wholeState(got).output;
    if (JSON.stringify(actual) !== JSON.stringify(want.output)) {
      throw new Error(
        `${c.id} (running on): output is ${JSON.stringify(actual)}, not ${JSON.stringify(want.output)}`,
      );
    }
  }
}

for (const group of GROUPS) {
  describe(group, () => {
    for (const c of loadGroup(group)) {
      if (!c.at) {
        test(c.id, () => runCase(c), 60_000);
        continue;
      }
      for (const position of c.at) {
        test(`${c.id} @ tick ${position.ticks}`, () => runCase(c, position), 60_000);
      }
    }
  });
}
