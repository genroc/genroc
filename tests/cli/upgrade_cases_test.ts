import { afterEach, beforeAll, describe, test } from "vitest";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { load } from "js-yaml";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { buildGenrocBinary, startGenroc, tmpPath, type GenrocProcess } from "../helpers/server.ts";

/**
 * `genctl upgrade`, asserted as the whole rendered output — the same shape as the compat
 * cases next door, for the same reason: what an operator reads IS the deliverable, and
 * comparing the whole thing covers wording, ordering and exit code at once.
 *
 * What these cases have that compat's cannot is a RUNNING instance. An upgrade is about
 * state the engine produced, so each case starts one and drives it a stated number of
 * ticks before the upgrade — which is why every case gets its own manual-tick server
 * (`--poll 0`). "after 2 ticks" has to be an exact position, not a race with a poll loop.
 *
 * A case asserts that the upgrade SUCCEEDED and what the instance holds afterwards. It does
 * not assert the command's output: the migrated state is the deliverable here, and pinning
 * the rendering would only break on wording. One case per file in testdata/upgrade/<group>/.
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
  /**
   * The state the instance must actually be resting in when the upgrade runs. Without it a
   * case whose ticks never reach the state its prose describes passes for saying nothing —
   * and every case here exists BECAUSE of the state it rests in.
   */
  resting?: RestingState;
  /**
   * The state the instance must be in AFTER the upgrade. Without it a case asserts only what
   * the command PRINTED, and a migration that dropped half the context prints exactly the
   * same line — `context_keys` is what sees engine bookkeeping the definition never declares
   * (output_order, _external) going missing.
   */
  after?: RestingState;
  /** Arguments after `genctl upgrade`. */
  run: string[];
  /**
   * The move must be REFUSED: a non-zero exit, and the instance left exactly where it was. A
   * refusal that still wrote something is worse than one that failed loudly, so `after` is
   * what carries the case — it pins the version the instance is still on.
   */
  refused?: { /** A fragment the refusal must name, so it points at the real reason. */ says?: string };
  /**
   * Run the whole case once per tick position, 0 through `through`. One case per position is
   * what a sweep buys: an upgrade is correct only if it is correct from EVERY state the process
   * passes through, and a single hand-picked tick proves it for one of them. A failure names the
   * position, so the state that broke is the state in the title.
   */
  sweep?: {
    through: number;
    /**
     * The task the instance must be sitting on at each position, indexed by tick. Without it a
     * sweep whose ticks all land in the same place passes for saying nothing — the same way a
     * case with no `resting` does, and the reason every case here has one.
     */
    positions?: string[];
  };
  /**
   * Drive the upgraded instance on and say where it must land. Everything above proves only
   * that the migrated state LOOKS right; a migration that breaks the very next tick — a slot
   * the new version reads and the old one never wrote, an output that no longer conforms —
   * satisfies every assertion before this one and fails here.
   */
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
  /**
   * Dotted paths into the stored context and the values they must hold. JSON-compared, so an
   * expected `null` fails against a MISSING key — which is the whole point where a migration
   * has to turn an absent optional into an explicit null.
   */
  values?: Record<string, unknown>;
  /** The version the row is on — the one thing a refused move must not have changed. */
  version?: number;
  wait_state?: string;
  outputs?: string[];
  /** Top-level keys of the stored state, sorted. */
  state_keys?: string[];
}

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
let genrocBin: string;
let server: GenrocProcess | undefined;
let port = 8971;

beforeAll(async () => {
  genctlBin = buildGenctlBinary();
  genrocBin = await buildGenrocBinary();
}, 90_000);

afterEach(async () => {
  await server?.stop();
  server = undefined;
});

async function runCase(c: UpgradeCase, ticksOverride?: number): Promise<void> {
  const ticks = ticksOverride ?? c.start.ticks ?? 0;
  // Its own server, in manual-tick mode: the case names how many steps the instance has
  // taken, and only a server that takes no step on its own can honour that.
  const at = port++;
  server = await startGenroc(genrocBin, at, tmpPath("upgrade_case", ".db"), undefined, 0, 4);
  const env = { GENROC_SERVER: `http://localhost:${at}` };

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

  /** Compares the instance's live state against what the case declares. */
  async function assertState(label: string, want: RestingState) {
    const { data } = await server!.client.GET("/instances/{id}/detail", {
      params: { path: { id: instanceID } },
    });
    const got = data as unknown as Record<string, unknown>;
    const ctx = (got.state ?? {}) as Record<string, unknown>;
    const outs = (ctx.outputs ?? {}) as Record<string, unknown>;
    const actual = {
      task: got.task,
      status: got.status,
      version: got.version,
      wait_state: got.wait_state ?? "",
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
    if (want.wait_state !== undefined && actual.wait_state !== want.wait_state) {
      throw mismatch("wait_state", actual.wait_state, want.wait_state);
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

  if (c.resting) await assertState(`after ${ticks} tick(s)`, c.resting);
  if (c.sweep?.positions) {
    await assertState(`after ${ticks} tick(s)`, { task: c.sweep.positions[ticks] });
  }

  for (const step of c.apply.slice(1)) {
    const next = runCli(genctlBin, ["apply", "-f", writeDefs(step.definitions)], env);
    if (!next.ok) throw new Error(`apply failed for ${c.id}: ${next.stderr}`);
  }

  const res = runCli(genctlBin, ["upgrade", ...c.run], env);
  if (c.refused) {
    if (res.ok) {
      throw new Error(`${c.id}: the move was expected to be refused, but exited 0\n${res.stdout}`);
    }
    const said = res.stdout + res.stderr;
    if (c.refused.says && !said.includes(c.refused.says)) {
      throw new Error(`${c.id}: refused, but not for the stated reason — wanted ${JSON.stringify(c.refused.says)} in:\n${said}`);
    }
  } else if (!res.ok) {
    throw new Error(`${c.id}: upgrade failed (exit ${res.exitCode})\n${res.stdout}${res.stderr}`);
  }
  // The state is the deliverable, not the rendering: what the command PRINTED is compat's
  // business, and asserting it here would break on wording that changes nothing.
  if (c.after) await assertState("after the upgrade", c.after);

  if (!c.finish) return;
  const want = c.finish;
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
    const actual = ((got.state ?? {}) as Record<string, unknown>).output;
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
      if (!c.sweep) {
        test(c.id, () => runCase(c), 60_000);
        continue;
      }
      for (let k = 0; k <= c.sweep.through; k++) {
        test(`${c.id} @ tick ${k}`, () => runCase(c, k), 60_000);
      }
    }
  });
}
