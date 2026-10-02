import type { TestProject } from "vitest/node";
import type { GenrocProcess } from "./server.ts";
import { buildGenrocBinary, startGenroc } from "./server.ts";

// globalSetup runs in the main vitest process, not in a project worker,
// so project-level env vars (GENROC_PORT) are not available here.
const PG_PORT = 8889;

let server: GenrocProcess | null = null;

export async function setup(project: TestProject) {
  const dsn = process.env.POSTGRES_DSN;
  if (!dsn)
    throw new Error("POSTGRES_DSN must be set for the postgres test project");

  const bin = await buildGenrocBinary();
  project.provide("genrocBin", bin);
  server = await startGenroc({ bin, port: PG_PORT, pg: dsn });
}

// Awaited: the stress project runs right after, and a worker still draining here would be a
// foreign processor in suites that need the database to themselves.
export async function teardown() {
  await server?.stop();
}
