import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { uid } from "../helpers/genctl.ts";
import { client } from "../helpers/client.ts";

/**
 * `genctl upgrade <process> --from --to`: the sweep across a fleet.
 *
 * The server moves one tree per call and only settles rows, so finding the roots, pausing
 * the running ones and putting them back is the client's job. What this covers is the
 * sweep's own behaviour — which instances it selects, and that it leaves nothing paused.
 */

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

/** A process parked on an external task, so instances stay live and selectable. */
function parkedDef(name: string, requireNote: boolean) {
  return {
    name,
    input_schema: {
      type: "object",
      properties: { note: { type: ["string", "null"] } },
      ...(requireNote ? { required: ["note"] } : {}),
    },
    // `raises` is a closed set on an external task: an undeclared code cannot be
    // submitted, so failing one in a test means declaring what it may raise.
    tasks: [
      { id: "hold", action: { type: "external", raises: { boom: null } }, switch: "end" },
    ],
  };
}

async function startParked(name: string): Promise<string> {
  const { data, error } = await client.POST("/instances", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: { process: name, input: {} } as any,
  });
  expect(error).toBeUndefined();
  const id = data!.id;
  for (let i = 0; i < 100; i++) {
    const r = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
    if (r.data?.wait_state === "external") return id;
    await new Promise((res) => setTimeout(res, 50));
  }
  throw new Error(`instance ${id} never parked`);
}

test("sweeps every live root of a process and leaves none paused", async () => {
  const name = uid("sweep");
  runCli(bin, ["apply", "-f", writeDefs([parkedDef(name, false)])]);
  const ids = [await startParked(name), await startParked(name)];

  runCli(bin, ["apply", "-f", writeDefs([parkedDef(name, true)])]);

  const run = runCli(bin, ["upgrade", name, "--from", "1", "--to", "2"]);
  expect(run.ok).toBe(true);
  expect(run.stderr).toContain("moved 2 tree(s)");

  for (const id of ids) {
    const r = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
    expect(r.data!.version).toBe(2);
    // Paused to be moved, and put back: an instance the sweep paused must not stay paused.
    expect(r.data!.status).toBe("running");
    // The migration closed the gap v2 opened.
    expect(r.data!.state?.input).toHaveProperty("note", null);
  }
});

test("selects only instances on the --from version", async () => {
  const name = uid("sweepsel");
  runCli(bin, ["apply", "-f", writeDefs([parkedDef(name, false)])]);
  const old = await startParked(name);

  // A genuinely different document: an identical one is deduped and no v2 exists, which
  // would leave both instances on v1 and the sweep selecting two.
  const v2 = parkedDef(name, false);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  (v2.input_schema.properties as any).extra = { type: ["string", "null"] };
  runCli(bin, ["apply", "-f", writeDefs([v2])]);
  const already = await startParked(name); // starts on v2

  const r = runCli(bin, ["upgrade", name, "--from", "1", "--to", "2"]);
  expect(r.ok).toBe(true);
  expect(r.stderr).toContain("moved 1 tree(s)");

  const a = await client.GET("/instances/{id}/detail", { params: { path: { id: old } } });
  expect(a.data!.version).toBe(2);
  const b = await client.GET("/instances/{id}/detail", { params: { path: { id: already } } });
  expect(b.data!.version).toBe(2); // untouched, and it was never selected
});

test("both sides are required, and identical sides are refused", () => {
  const name = uid("sweepargs");
  expect(runCli(bin, ["upgrade", name, "--from", "1"]).ok).toBe(false);
  expect(runCli(bin, ["upgrade", name, "--from", "1", "--to", "1"]).stderr).toContain(
    "nothing to move",
  );
});

test("sweeps failed instances too, and --status narrows what it takes", async () => {
  const name = uid("sweepfail");
  runCli(bin, ["apply", "-f", writeDefs([parkedDef(name, false)])]);
  const live = await startParked(name);

  // A failed root: settled, so it moves with no pause/resume dance. It is the case an
  // upgrade is most FOR — move it, then retry it on the new version.
  const failedId = await startParked(name);
  const failRes = await client.POST("/instances/{id}/signal", {
    params: { path: { id: failedId } },
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: { task_id: "hold", error: { code: "boom", message: "x" } } as any,
  });
  expect(failRes.error).toBeUndefined();
  for (let i = 0; i < 100; i++) {
    const r = await client.GET("/instances/{id}/detail", { params: { path: { id: failedId } } });
    if (r.data?.status === "failed") break;
    await new Promise((res) => setTimeout(res, 50));
  }

  const v2 = parkedDef(name, false);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  (v2.input_schema.properties as any).extra = { type: ["string", "null"] };
  runCli(bin, ["apply", "-f", writeDefs([v2])]);

  // Narrowed to failed: the running one must be left alone.
  const only = runCli(bin, ["upgrade", name, "--from", "1", "--to", "2", "--status", "failed"]);
  expect(only.ok).toBe(true);
  expect(only.stderr).toContain("moved 1 tree(s)");

  const stillV1 = await client.GET("/instances/{id}/detail", { params: { path: { id: live } } });
  expect(stillV1.data!.version).toBe(1);

  // Default takes everything movable, so the running one goes now.
  const rest = runCli(bin, ["upgrade", name, "--from", "1", "--to", "2"]);
  expect(rest.ok).toBe(true);
  const movedNow = await client.GET("/instances/{id}/detail", { params: { path: { id: live } } });
  expect(movedNow.data!.version).toBe(2);
});

test("--status rejects a state that cannot move", () => {
  const r = runCli(bin, ["upgrade", uid("s"), "--from", "1", "--to", "2", "--status", "completed"]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("move no work");
});
