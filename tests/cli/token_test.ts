import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli } from "../helpers/cli.ts";
import { BASE_URL } from "../helpers/constants.ts";

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

test("token revoke takes flags after the ids", () => {
  const created = runCli(bin, ["token", "create", "--perms", "read", "--label", "cli"]);
  expect(created.ok, created.stderr).toBe(true);
  const id = /created (\S+)/.exec(created.stdout + created.stderr)![1];

  // flag.Parse stops at the first positional, which read --server as a second id to revoke.
  const revoked = runCli(bin, ["token", "revoke", id, "--server", BASE_URL]);
  expect(revoked.ok, revoked.stderr).toBe(true);
  expect(revoked.stdout.trim()).toBe(`revoked: ${id}`);
});

test("token — an unknown subcommand lists every real one, and a stray argument is refused", () => {
  const unknown = runCli(bin, ["token", "mint"]);
  expect(unknown.ok).toBe(false);
  expect(unknown.stderr, "generate is the one that makes the first token offline").toContain("generate");

  const stray = runCli(bin, ["token", "list", "extra"]);
  expect(stray.ok, "an argument list ignores is refused, not dropped").toBe(false);
  expect(stray.stderr).toContain("takes no arguments");
});
