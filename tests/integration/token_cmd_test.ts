import { spawnSync } from "child_process";
import { existsSync, mkdirSync, mkdtempSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { expect, test } from "vitest";
import { getBin } from "../helpers/server.ts";

// `genroc token` works on the database directly, so a flag that goes unread opens another one.
test("token revoke takes --db after the id, as its usage line writes it", async () => {
  const bin = await getBin();
  const cwd = mkdtempSync(join(tmpdir(), "genroc_token_cmd_"));
  mkdirSync(join(cwd, "data"));
  const dbPath = join(cwd, "data", "tokens.db");
  const env = { ...process.env };
  delete env.GENROC_DB;
  delete env.GENROC_PG;
  const run = (...args: string[]) => spawnSync(bin, ["token", ...args], { cwd, env, encoding: "utf8" });

  const created = run("create", "--perms", "read", "--label", "ci", "--db", dbPath);
  expect(created.status, created.stderr).toBe(0);
  const id = /created (\S+)/.exec(created.stderr)![1];

  const revoked = run("revoke", id, "--db", dbPath);
  expect(revoked.status, revoked.stderr).toBe(0);
  expect(run("list", "--db", dbPath).stdout).toMatch(new RegExp(`${id}\\s.*revoked`));
  expect(existsSync(join(cwd, "genroc.db")), "--db was read as an id, and the default database opened in its place")
    .toBe(false);

  const stray = run("list", "extra", "--db", dbPath);
  expect(stray.status, "an argument list takes none of is refused, not dropped").toBe(2);
});
