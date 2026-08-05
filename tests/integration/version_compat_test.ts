import { expect, test } from "vitest";
import { client } from "../helpers/client.ts";

// Two versions of a process, and whether an instance running one could continue under the
// other. The endpoint is a shape check over two documents: it catches an output whose
// type changed or an input that became required, and cannot see a change of meaning.
// Design: specs/version-compatibility.md.

const uid = (p: string) => `${p}_${crypto.randomUUID().replace(/-/g, "").slice(0, 8)}`;

/** Parks on an external task, so the instance sits at a known task indefinitely. */
function parkingDef(name: string, shipURL: string, inputSchema?: Record<string, unknown>) {
  return {
    name,
    ...(inputSchema ? { input_schema: inputSchema } : {}),
    tasks: [
      {
        id: "approve",
        action: {
          type: "external" as const,
          result_schema: {
            type: "object",
            properties: { ok: { type: "boolean" } },
            required: ["ok"],
          },
        },
        output: { ok: "$: self.result.ok" },
        switch: [{ goto: "next" }],
      },
      {
        id: "ship",
        action: { type: "fetch" as const, url: shipURL },
        switch: [{ goto: "end" }],
      },
    ],
  };
}

async function apply(def: Record<string, unknown>): Promise<number> {
  const { data, error } = await client.PUT("/definitions", { body: def as never });
  if (error) throw new Error(`apply failed: ${JSON.stringify(error)}`);
  return (data as { version: number }).version;
}

async function compat(body: Record<string, unknown>) {
  const { data, error } = await client.POST("/definitions/compat", { body: body as never });
  if (error) throw new Error(`compat failed: ${JSON.stringify(error)}`);
  return data as never as {
    compatible: boolean;
    processes: {
      name: string;
      from: number | null;
      to: number | null;
      compatible: boolean;
      output_compatible: boolean;
      input: { compatible: boolean; reason?: string };
      tasks: { task: string; compatible: boolean; reason?: string; changed?: string[] }[];
      removed_tasks?: string[];
      changed?: string[];
    }[];
    unpaired?: { name: string; side: string }[];
  };
}

const versions = (name: string, v: number) => ({ versions: { [name]: v } });

test("compat — a changed URL is compatible and reported as a changed slot", async () => {
  const name = uid("compat_url");
  const v1 = await apply(parkingDef(name, "http://localhost:9001/ship"));
  const v2 = await apply(parkingDef(name, "http://localhost:9002/ship"));

  const r = await compat({ from: versions(name, v1), to: versions(name, v2) });
  expect(r.compatible).toBe(true);

  // The verdict is blind to meaning, so the slot list is the deliverable: it is what
  // lets a reader judge a change the shape check cannot see.
  const ship = r.processes[0].tasks.find((t) => t.task === "ship");
  expect(ship?.changed).toContain("action.url");
});

test("compat — an input property that became required is refused and named", async () => {
  const name = uid("compat_input");
  const v1 = await apply(parkingDef(name, "http://localhost:9001/ship"));
  const v2 = await apply(
    parkingDef(name, "http://localhost:9001/ship", {
      type: "object",
      properties: { currency: { type: "string" } },
      required: ["currency"],
    }),
  );

  const r = await compat({ from: versions(name, v1), to: versions(name, v2) });
  expect(r.compatible).toBe(false);
  expect(r.processes[0].input.compatible).toBe(false);
  expect(r.processes[0].input.reason).toContain("newly required");
  // Hoisted: input sits in every task's context, so the break is reported once.
  expect(r.processes[0].tasks.every((t) => t.compatible)).toBe(true);
});

test("compat — submitted documents report a null target version", async () => {
  const name = uid("compat_docs");
  const v1 = await apply(parkingDef(name, "http://localhost:9001/ship"));

  const r = await compat({
    from: versions(name, v1),
    to: { definitions: [parkingDef(name, "http://localhost:9003/ship")] },
  });
  expect(r.compatible).toBe(true);
  expect(r.processes[0].from).toBe(v1);
  // The dominant workflow: compare what apply would take against what is deployed,
  // before deploying it. Those documents have no version yet.
  expect(r.processes[0].to).toBeNull();
});

test("compat — a name on one side only is reported unpaired, never dropped", async () => {
  const a = uid("compat_a");
  const b = uid("compat_b");
  const va = await apply(parkingDef(a, "http://localhost:9001/ship"));
  await apply(parkingDef(b, "http://localhost:9001/ship"));

  const r = await compat({
    from: { versions: { [a]: va, [b]: 1 } },
    to: versions(a, va),
  });
  // Silence would read as agreement, so an unpaired name makes the roll-up false.
  expect(r.compatible).toBe(false);
  expect(r.unpaired?.map((u) => u.name)).toContain(b);
});

test("compat — naming both sides is required", async () => {
  const { error } = await client.POST("/definitions/compat", {
    body: { to: { channel: "latest" } } as never,
  });
  // Defaulting one side would hide which two documents were compared.
  expect(error).toBeTruthy();
});
