import { mkdtempSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli } from "../helpers/cli.ts";

// The config entity: `genctl config get/set`, the CLI's own persisted state. Each test
// works in a throwaway config home, so nothing here touches the one the other suites
// share for @last.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

/** A pristine config home, so a test sees no key any other test wrote. */
function freshHome(): Record<string, string> {
  const home = mkdtempSync(join(tmpdir(), "genroc_cfg_"));
  return { HOME: home, XDG_CONFIG_HOME: join(home, ".config") };
}

test("config set then get — the value round-trips through the config file", () => {
  const env = freshHome();
  const url = "http://example.invalid:9999";

  const set = runCli(bin, ["config", "set", "server", url], env);
  expect(set.ok).toBe(true);
  expect(set.stdout).toContain(url);

  // A separate process reads it back, so the value came from the file and not memory.
  const get = runCli(bin, ["config", "get", "server"], env);
  expect(get.ok).toBe(true);
  expect(get.stdout.trim()).toBe(url);
});

test("config get — an unset key reports (not set) rather than an empty line", () => {
  const r = runCli(bin, ["config", "get", "server"], freshHome());
  expect(r.ok).toBe(true);
  expect(r.stdout).toContain("(not set)");
});

test("config — an unknown key is rejected by both get and set", () => {
  const env = freshHome();
  for (const args of [
    ["config", "get", "nonsense"],
    ["config", "set", "nonsense", "x"],
  ]) {
    const r = runCli(bin, args, env);
    expect(r.ok, `${args.join(" ")} should fail`).toBe(false);
    expect(r.stderr).toContain("unknown config key");
  }
});

test("$GENROC_SERVER wins over the config file, and --server over both", () => {
  const env = freshHome();
  runCli(bin, ["config", "set", "server", "http://config.invalid:1"], env);

  // The environment overrides the stored value…
  const fromEnv = runCli(bin, ["instances"], { ...env, GENROC_SERVER: "http://env.invalid:1" });
  expect(fromEnv.ok).toBe(false);
  expect(fromEnv.stderr).toContain("env.invalid");

  // …and the flag overrides the environment.
  const fromFlag = runCli(bin, ["instances", "--server", "http://flag.invalid:1"], {
    ...env,
    GENROC_SERVER: "http://env.invalid:1",
  });
  expect(fromFlag.ok).toBe(false);
  expect(fromFlag.stderr).toContain("flag.invalid");
});

test("config — the stored server is used when the environment says nothing", () => {
  const env = freshHome();
  runCli(bin, ["config", "set", "server", "http://stored.invalid:1"], env);

  // runCli always sets GENROC_SERVER, so clear it to reach the file's value.
  const r = runCli(bin, ["instances"], { ...env, GENROC_SERVER: "" });
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("stored.invalid");
});
