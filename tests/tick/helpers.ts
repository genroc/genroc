import { beforeAll, afterAll } from "vitest";
import {
  buildGenrocBinary,
  startGenroc,
  tmpPath,
  type GenrocProcess,
} from "../helpers/server.ts";

// Cached binary — built once per Vitest worker process.
let _bin: string | null = null;
async function getBin(): Promise<string> {
  if (!_bin) _bin = await buildGenrocBinary();
  return _bin;
}

export class TickEnv {
  constructor(private readonly genroc: GenrocProcess) {}

  get client() {
    return this.genroc.client;
  }

  // Advance one engine poll cycle. Returns the number of instances processed.
  async tick(): Promise<number> {
    const { data, error } = await this.genroc.client.POST("/tick", {});
    if (error) throw new Error(`tick failed: ${JSON.stringify(error)}`);
    return (data as { count: number }).count;
  }

  // Tick until no instances are processed in a cycle (fully settled).
  async tickUntilIdle(maxTicks = 20): Promise<void> {
    for (let i = 0; i < maxTicks; i++) {
      if ((await this.tick()) === 0) return;
    }
    throw new Error(`still active after ${maxTicks} ticks`);
  }

  async status(id: string): Promise<string> {
    const { data, error } = await this.genroc.client.GET("/instances/{id}", {
      params: { path: { id } },
    });
    if (error)
      throw new Error(`status(${id}) failed: ${JSON.stringify(error)}`);
    return `${data!.status} ${data!.wait_state ?? ""}`.trim() as string;
  }

  async waitState(id: string): Promise<string> {
    const { data, error } = await this.genroc.client.GET("/instances/{id}", {
      params: { path: { id } },
    });
    if (error)
      throw new Error(`waitState(${id}) failed: ${JSON.stringify(error)}`);
    return (data!.wait_state as string) ?? "";
  }

  // Check statuses for a labelled map of instance IDs.
  // Usage: env.statuses({ gp: gpId, parent: parentId, a: aId, b: bId })
  async statuses(
    tree: Record<string, string>,
  ): Promise<Record<string, string>> {
    const entries = await Promise.all(
      Object.entries(tree).map(
        async ([label, id]) => [label, await this.status(id)] as const,
      ),
    );
    return Object.fromEntries(entries);
  }

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  async define(name: string, tasks: object[]): Promise<void> {
    const { error } = await this.genroc.client.PUT("/definitions", {
      body: { name, tasks } as any,
    });
    if (error)
      throw new Error(`define(${name}) failed: ${JSON.stringify(error)}`);
  }

  async start(process: string): Promise<string> {
    const { data, error } = await this.genroc.client.POST("/instances", {
      body: { process },
    });
    if (error)
      throw new Error(`start(${process}) failed: ${JSON.stringify(error)}`);
    return data!.id;
  }

  async pause(id: string): Promise<void> {
    const { error } = await this.genroc.client.POST("/instances/{id}/pause", {
      params: { path: { id } },
    });
    if (error) throw new Error(`pause(${id}) failed: ${JSON.stringify(error)}`);
  }

  async resume(id: string): Promise<void> {
    const { error } = await this.genroc.client.POST("/instances/{id}/resume", {
      params: { path: { id } },
    });
    if (error) throw new Error(`resume(${id}) failed: ${JSON.stringify(error)}`);
  }

  async retry(id: string): Promise<void> {
    const { error } = await this.genroc.client.POST("/instances/{id}/retry", {
      params: { path: { id } },
    });
    if (error) throw new Error(`retry(${id}) failed: ${JSON.stringify(error)}`);
  }

  // The instance's consumed on_error attempts — how much of the definition's retry
  // budget has been spent. Pausing must leave this untouched.
  async retryCount(id: string): Promise<number> {
    const { data, error } = await this.genroc.client.GET("/instances/{id}", {
      params: { path: { id } },
    });
    if (error)
      throw new Error(`retryCount(${id}) failed: ${JSON.stringify(error)}`);
    return data!.retry_count as number;
  }

  // Returns the child instance ID recorded under the parent's "_children" key
  // after SpawnChildrenAndWait. Valid between spawn and child completion.
  async childOf(parentId: string, taskId: string): Promise<string> {
    const { data } = await this.genroc.client.GET("/instances/{id}", {
      params: { path: { id: parentId } },
    });
    const spawned = (data!.context as Record<string, unknown> | null)
      ?._children as Record<string, unknown> | null;
    const val = spawned?.[taskId];
    // A single child is expressed as a one-entry child_map, so its placeholder is a
    // keyed object with exactly one id — unwrap it to the lone child id.
    if (val && typeof val === "object" && !Array.isArray(val)) {
      const ids = Object.values(val as Record<string, unknown>);
      if (ids.length === 1 && typeof ids[0] === "string") {
        return ids[0];
      }
    }
    throw new Error(
      `childOf(${parentId}, ${taskId}): expected a single-entry child placeholder, got ${JSON.stringify(val)}`,
    );
  }

  // Returns the parallel child IDs keyed by child key, recorded under the
  // parent's "_children" key after SpawnChildrenAndWait.
  async childrenOf(
    parentId: string,
    taskId: string,
  ): Promise<Record<string, string>> {
    const { data } = await this.genroc.client.GET("/instances/{id}", {
      params: { path: { id: parentId } },
    });
    const spawned = (data!.context as Record<string, unknown> | null)
      ?._children as Record<string, unknown> | null;
    const val = spawned?.[taskId];
    if (typeof val !== "object" || val === null) {
      throw new Error(
        `childrenOf(${parentId}, ${taskId}): expected object placeholder, got ${JSON.stringify(val)}`,
      );
    }
    return val as Record<string, string>;
  }

  // Returns the child_list child IDs in spawn (input) order, recorded as an array
  // under the parent's "_children" key after SpawnChildrenAndWait.
  async listChildrenOf(parentId: string, taskId: string): Promise<string[]> {
    const { data } = await this.genroc.client.GET("/instances/{id}", {
      params: { path: { id: parentId } },
    });
    const spawned = (data!.context as Record<string, unknown> | null)
      ?._children as Record<string, unknown> | null;
    const val = spawned?.[taskId];
    if (!Array.isArray(val)) {
      throw new Error(
        `listChildrenOf(${parentId}, ${taskId}): expected array placeholder, got ${JSON.stringify(val)}`,
      );
    }
    return val as string[];
  }

  stop() {
    this.genroc.stop();
  }
}

// Registers beforeAll/afterAll to start a fresh tick-mode server on the given port.
// The returned object is populated before tests run.
//
// Usage:
//   const ctx = useTickEnv(20014);
//   test("...", async () => { await ctx.env.tick(); });
// Pass immediateRetries: false to keep the real backoff, so a test can advance the clock
// across a retry timer and observe how long the policy actually parked for.
export function useTickEnv(port: number, opts: { immediateRetries?: boolean } = {}) {
  const ctx = {} as { env: TickEnv };
  const { immediateRetries = true } = opts;

  beforeAll(async () => {
    const bin = await getBin();
    const db = tmpPath("genroc_tick", ".db");
    // poll=0 → manual tick mode; max-concurrent=1 → one instance per tick (predictable ordering)
    // immediateRetries=true → no backoff, retries are claimable on the very next tick
    const genroc = await startGenroc(bin, port, db, undefined, 0, 1, immediateRetries);
    ctx.env = new TickEnv(genroc);
  }, 60_000);

  afterAll(() => ctx.env?.stop());

  return ctx;
}
