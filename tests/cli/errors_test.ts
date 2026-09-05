import { beforeAll, expect, test } from "vitest";
import { buildGenctlBinary, runCli, writeDefs } from "../helpers/cli.ts";
import { waitForInstance } from "../helpers/client.ts";
import { missingID, switchDef, uid } from "../helpers/genctl.ts";

// How genctl fails. Every command routes its failures through fatal(), so the contract is
// narrow and worth pinning: a "genctl: " prefix on stderr, nothing on stdout, and a
// non-zero exit — which is what makes the CLI usable in a script rather than only by eye.
//
// The exceptions are as interesting as the rule, so they are asserted too: an empty list
// is a success, and flag parsing exits 2 through Go's own flag package rather than 1.

let bin: string;
beforeAll(() => {
  bin = buildGenctlBinary();
}, 60_000);

// ── the shape of a failure ──────────────────────────────────────────────────────

test("errors — stderr carries a genctl: prefix, stdout stays clean, exit is non-zero", () => {
  const r = runCli(bin, ["get", missingID]);
  expect(r.exitCode).toBe(1);
  expect(r.stderr.startsWith("genctl: ")).toBe(true);
  // Nothing on stdout: a caller piping this into jq or a variable gets emptiness, not a
  // half-written record followed by an error.
  expect(r.stdout).toBe("");
});

test("an unknown command prints the usage and exits 1", () => {
  const r = runCli(bin, ["bogus-command"]);
  expect(r.exitCode).toBe(1);
  expect(r.stderr).toContain(`unknown command "bogus-command"`);
  // The usage follows, so the error is self-correcting rather than a dead end.
  expect(r.stderr).toContain("genctl apply");
  expect(r.stderr).toContain("genctl logs");
});

test("no arguments at all prints the usage and exits 1", () => {
  const r = runCli(bin, []);
  expect(r.exitCode).toBe(1);
  expect(r.stderr).toContain("Usage:");
});

test("an unknown flag exits 2 and lists the flags the command does take", () => {
  const r = runCli(bin, ["instances", "--bogus-flag"]);
  // 2, not 1: Go's flag package handles this before any genctl code runs.
  expect(r.exitCode).toBe(2);
  expect(r.stderr).toContain("not defined: -bogus-flag");
  expect(r.stderr).toContain("-since");
});

// ── unknown instance ids ────────────────────────────────────────────────────────

test("instance verbs reject an id that does not exist", () => {
  for (const cmd of ["get", "pause", "resume", "retry"]) {
    const r = runCli(bin, [cmd, missingID]);
    expect(r.exitCode, `${cmd} should fail on a missing id`).toBe(1);
    expect(r.stderr, `${cmd} should say what was not found`).toContain("not found");
  }
});

test("logs on a missing id is silently empty, unlike every other instance verb", () => {
  // Documenting a real gap rather than endorsing it: the logs listing filters on
  // instance_id and finds nothing, so a typo'd id is indistinguishable from an instance
  // that simply has no trail yet. get/pause/resume/retry all 404 on the same id.
  const r = runCli(bin, ["logs", missingID]);
  expect(r.exitCode).toBe(0);
  expect(r.stdout).toBe("");
  expect(r.stderr).toBe("");
});

test("@last errors when the recorded instance is not on this server", () => {
  // @last resolves from the CLI's own config file, so it can name an id the server has
  // never seen — the error must come from the server, not from a nil id.
  const r = runCli(bin, ["get", "@last"], { GENROC_SERVER: "http://127.0.0.1:8448" });
  expect(r.ok).toBe(false);
});

// ── missing required arguments ──────────────────────────────────────────────────

test("commands that need an argument say which one", () => {
  const cases: { args: string[]; want: string }[] = [
    { args: ["apply"], want: "no files given" },
    { args: ["apply", "--check-only"], want: "no files given" },
    { args: ["run"], want: "usage: genctl run" },
    { args: ["channel"], want: "Usage: genctl channel" },
    { args: ["channel", "list"], want: "usage: genctl channel list" },
    { args: ["channel", "set", "p"], want: "usage: genctl channel set" },
    { args: ["channel", "delete", "p"], want: "usage: genctl channel delete" },
    { args: ["promote", "--from", "a"], want: "--from and --to are required" },
    { args: ["get"], want: "an instance id is required" },
    { args: ["resolve"], want: "usage: genctl resolve" },
  ];
  for (const { args, want } of cases) {
    const r = runCli(bin, args);
    expect(r.ok, `${args.join(" ")} should fail`).toBe(false);
    expect(r.stderr, `${args.join(" ")} should explain what is missing`).toContain(want);
  }
});

test("channel — an unknown subcommand is named, not silently ignored", () => {
  const r = runCli(bin, ["channel", "frobnicate", "p"]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("unknown channel subcommand");
});

// ── file and connection failures ────────────────────────────────────────────────

test("apply -f — a missing file names the path", () => {
  const r = runCli(bin, ["apply", "-f", "/nope/definitely-missing.yaml"]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("/nope/definitely-missing.yaml");
  expect(r.stderr).toContain("no such file");
});

test("an unreachable server fails every list rather than printing an empty one", () => {
  const dead = "http://127.0.0.1:1";
  for (const args of [["instances"], ["definitions"], ["logs", missingID]]) {
    const r = runCli(bin, args, { GENROC_SERVER: dead });
    expect(r.ok, `${args[0]} should fail against a dead server`).toBe(false);
    // "no instances" would be a lie here — nothing was successfully read.
    expect(r.stderr).toContain("connect to server");
    expect(r.stdout).toBe("");
  }
});

test("--server overrides $GENROC_SERVER, and only after the subcommand", () => {
  // The flag belongs to the subcommand's flag set, so it must follow the verb; putting it
  // first makes it look like a command name.
  const first = runCli(bin, ["--server", "http://127.0.0.1:1", "instances"]);
  expect(first.ok).toBe(false);
  expect(first.stderr).toContain(`unknown command "--server"`);

  // In the right position it wins over the environment, which is what makes a one-off
  // command against another server possible.
  const after = runCli(bin, ["instances", "--server", "http://127.0.0.1:1"]);
  expect(after.ok).toBe(false);
  expect(after.stderr).toContain("connect to server");
});

// ── server-side rejections keep their message ───────────────────────────────────

test("a server validation message survives to stderr intact", () => {
  const name = uid("badinput");
  runCli(bin, [
    "apply",
    "-f",
    writeDefs([
      {
        name,
        input_schema: {
          type: "object",
          properties: { count: { type: "integer" } },
          required: ["count"],
        },
        tasks: [{ id: "s1", switch: [{ goto: "end" }] }],
      },
    ]),
  ]);

  // The server's own words, not a generic "bad request" — the field it objected to is
  // the only thing that tells the user what to change.
  const r = runCli(bin, ["run", name, "--set", "count=not-a-number"]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("count");
});

test("run — an unknown process is reported, not started", () => {
  const r = runCli(bin, ["run", uid("never_applied")]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("not found");
  expect(r.stdout).toBe("");
});

test("retry — refuses an instance that has not failed, and says why", async () => {
  const name = uid("retry_ok");
  runCli(bin, ["apply", "-f", writeDefs([switchDef(name)])]);
  const id = runCli(bin, ["run", name, "-q"]).stdout.trim();
  expect(await waitForInstance(id)).toBe("completed");

  const r = runCli(bin, ["retry", id]);
  expect(r.ok).toBe(false);
  expect(r.stderr).toContain("genctl: ");
});
