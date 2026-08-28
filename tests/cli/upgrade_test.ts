import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { uid } from "../helpers/genctl.ts";
import { client } from "../helpers/client.ts";

/**
 * `genctl upgrade`: the sweep across a fleet (`<process> --from --to`), and the single tree
 * an id names (`<instance-id> --to`).
 *
 * The server moves one tree per call and only settles rows, so finding the roots, pausing
 * the running ones and putting them back is the client's job. What this covers is the
 * sweep's own behaviour — which instances it selects, and that it leaves nothing paused —
 * and, for the id form, that it selects nothing at all: no --from, and the process the
 * channel resolves against comes off the row.
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

async function waitParked(id: string): Promise<string> {
  for (let i = 0; i < 100; i++) {
    const r = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
    if (r.data?.wait_state === "external") return id;
    await new Promise((res) => setTimeout(res, 50));
  }
  throw new Error(`instance ${id} never parked`);
}

async function startParked(name: string): Promise<string> {
  const { data, error } = await client.POST("/instances", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: { process: name, input: {} } as any,
  });
  expect(error).toBeUndefined();
  return waitParked(data!.id);
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
  const failRes = await client.POST("/external-tasks/signal", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: { instance_id: failedId, task_id: "hold", error: { code: "boom", message: "x" } } as any,
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

test("an id moves that one tree, with no --from and no process name", async () => {
  const name = uid("byid");
  runCli(bin, ["apply", "-f", writeDefs([parkedDef(name, false)])]);
  const target = await startParked(name);
  const bystander = await startParked(name);

  runCli(bin, ["apply", "-f", writeDefs([parkedDef(name, true)])]);

  const r = runCli(bin, ["upgrade", target, "--to", "2"]);
  expect(r.ok, `upgrade by id failed: ${r.stderr}`).toBe(true);
  expect(r.stdout).toContain(target);

  const moved = await client.GET("/instances/{id}/detail", { params: { path: { id: target } } });
  expect(moved.data!.version).toBe(2);
  expect(moved.data!.status, "paused to be moved, and not put back").toBe("running");
  expect(moved.data!.state?.input).toHaveProperty("note", null);

  // The id selects one tree and nothing else: a sibling on the same version is not a
  // candidate the way it would be under --from.
  const left = await client.GET("/instances/{id}/detail", { params: { path: { id: bystander } } });
  expect(left.data!.version, "the id form swept a sibling it was not given").toBe(1);
});

test("@last names the instance, and its process resolves the --to channel", async () => {
  const name = uid("byidlast");
  runCli(bin, ["apply", "-f", writeDefs([parkedDef(name, false)])]);
  const started = runCli(bin, ["run", name, "--input", "{}", "-q"]);
  expect(started.ok, started.stderr).toBe(true);
  const id = await waitParked(started.stdout.trim());

  runCli(bin, ["apply", "-f", writeDefs([parkedDef(name, true)]), "--channel", "stable"]);

  const r = runCli(bin, ["upgrade", "@last", "--to", "stable"]);
  expect(r.ok, `upgrade @last --to stable failed: ${r.stderr}`).toBe(true);

  const row = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
  expect(row.data!.version, "a channel must resolve against the process on the row").toBe(2);
});

test("an id takes no --status, is already-there rather than failed, and checks a stale --from", async () => {
  const name = uid("byidargs");
  runCli(bin, ["apply", "-f", writeDefs([parkedDef(name, false)])]);
  const id = await startParked(name);

  const withStatus = runCli(bin, ["upgrade", id, "--to", "2", "--status", "running"]);
  expect(withStatus.ok).toBe(false);
  expect(withStatus.stderr).toContain("already name the trees that move");

  // Idempotent: naming ids again after a partial run has to repair it, so a tree already on
  // the target is reported and not counted against the exit code.
  const same = runCli(bin, ["upgrade", id, "--to", "1"]);
  expect(same.ok, `already-there was treated as a failure: ${same.stdout}`).toBe(true);
  expect(same.stdout).toContain("already on 1");
  expect(same.stderr).toContain("1 already there");

  // --from is optional here, but a wrong one is a stale view of the row, not a redundant
  // argument to drop: silently moving it anyway is the race the assertion exists to catch.
  const stale = runCli(bin, ["upgrade", id, "--from", "7", "--to", "2"]);
  expect(stale.ok).toBe(false);
  expect(stale.stdout).toContain("--from resolves to version 7");
});

test("several ids move several trees, and one refused does not stop the rest", async () => {
  const name = uid("byidmany");
  runCli(bin, ["apply", "-f", writeDefs([parkedDef(name, false)])]);
  const first = await startParked(name);
  const stuck = await startParked(name);
  const second = await startParked(name);
  const unnamed = await startParked(name);

  // Completed moves no work, so it is the refusal — named BETWEEN the two that can move, so
  // an abort would leave `second` behind and the count would say so.
  const done = await client.POST("/external-tasks/signal", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: { instance_id: stuck, task_id: "hold", result: {} } as any,
  });
  expect(done.error).toBeUndefined();
  for (let i = 0; i < 100; i++) {
    const r = await client.GET("/instances/{id}/detail", { params: { path: { id: stuck } } });
    if (r.data?.status === "completed") break;
    await new Promise((res) => setTimeout(res, 50));
  }

  runCli(bin, ["apply", "-f", writeDefs([parkedDef(name, true)])]);

  const r = runCli(bin, ["upgrade", first, stuck, second, "--to", "2"]);
  expect(r.ok, "a refusal among the ids must carry the exit code").toBe(false);
  expect(r.stdout).toContain("status is completed");
  expect(r.stderr).toContain("moved 2 tree(s) to 2");
  expect(r.stderr).toContain("1 refused");

  for (const id of [first, second]) {
    const row = await client.GET("/instances/{id}/detail", { params: { path: { id } } });
    expect(row.data!.version, `${id} was named and did not move`).toBe(2);
    expect(row.data!.status).toBe("running");
  }
  const left = await client.GET("/instances/{id}/detail", { params: { path: { id: unnamed } } });
  expect(left.data!.version, "an id list must move only what it names").toBe(1);
});

test("a process name and an id cannot be mixed", () => {
  const r = runCli(bin, [
    "upgrade",
    uid("mixed"),
    "550e8400-e29b-41d4-a716-446655440000",
    "--to",
    "2",
  ]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("usage: genctl upgrade");
});

test("a child id is refused, and is not paused on the way", async () => {
  const kid = uid("kid");
  const boss = uid("boss");
  runCli(bin, [
    "apply",
    "-f",
    writeDefs([
      parkedDef(kid, false),
      {
        name: boss,
        tasks: [
          {
            id: "call",
            action: { type: "child", name: kid, input: {} },
            switch: "end",
          },
        ],
      },
    ]),
  ]);
  const { data, error } = await client.POST("/instances", {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    body: { process: boss, input: {} } as any,
  });
  expect(error).toBeUndefined();

  let childId = "";
  for (let i = 0; i < 100 && !childId; i++) {
    // children: true — the listing is roots only by default, and this process only ever
    // exists as a child.
    const list = await client.GET("/instances", {
      params: { query: { process: kid, children: true } },
    });
    const row = list.data?.items?.[0];
    if (row) childId = row.id;
    else await new Promise((res) => setTimeout(res, 50));
  }
  expect(childId, `no child instance of ${kid} appeared under ${data!.id}`).not.toBe("");
  await waitParked(childId);

  const r = runCli(bin, ["upgrade", childId, "--to", "2"]);
  expect(r.ok).toBe(false);
  expect(r.stdout).toContain("upgrade its root instead");

  const child = await client.GET("/instances/{id}/detail", { params: { path: { id: childId } } });
  expect(child.data!.status, "a refused child must not be left paused by the attempt").toBe(
    "running",
  );
});
