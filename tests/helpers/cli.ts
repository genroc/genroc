import { spawnSync } from "child_process";
import { join } from "path";
import { tmpdir } from "os";
import { mkdtempSync, writeFileSync } from "fs";
import { BASE_URL } from "./constants.ts";

const ROOT = new URL("../../", import.meta.url).pathname;

let cachedBin: string | null = null;

// genctl persists its "last started instance" id under the user config dir
// (os.UserConfigDir → $HOME/Library/... on macOS, $XDG_CONFIG_HOME on Linux). Point
// both at a throwaway dir so CLI tests exercise @last without touching the real
// machine config, and so a `run` in one test is visible to a later command in it.
const CLI_HOME = mkdtempSync(join(tmpdir(), "genroc_cli_home_"));

export function buildGenctlBinary(): string {
  if (cachedBin) return cachedBin;
  // Build genctl directly (like buildGenrocBinary builds genroc) rather than `make
  // build`, which also runs sqlc — and sqlc@v1.31.1 needs Go >= 1.26, triggering a
  // slow toolchain download on a fresh CI runner that blew the 10s test hook.
  // genctl is a pure client (no CGO needed), and gen/ is committed.
  const bin = join(ROOT, "genctl");
  const result = spawnSync("go", ["build", "-o", bin, "./cmd/genctl"], {
    cwd: ROOT,
    stdio: ["ignore", "ignore", "inherit"],
  });
  if (result.status !== 0) throw new Error("Failed to build genctl binary");
  cachedBin = bin;
  return cachedBin;
}

export interface CliResult {
  stdout: string;
  stderr: string;
  exitCode: number;
  ok: boolean;
}

export function runCli(
  bin: string,
  args: string[],
  env: Record<string, string> = {},
): CliResult {
  const result = spawnSync(bin, args, {
    env: {
      ...process.env,
      GENROC_SERVER: BASE_URL,
      HOME: CLI_HOME,
      XDG_CONFIG_HOME: join(CLI_HOME, ".config"),
      ...env,
    },
    encoding: "utf8",
  });
  return {
    stdout: result.stdout ?? "",
    stderr: result.stderr ?? "",
    exitCode: result.status ?? 1,
    ok: result.status === 0,
  };
}

/** Write one or more process definitions to a temp YAML file and return the path. */
export function writeDefs(defs: object[]): string {
  const path = join(
    tmpdir(),
    `genroc_cli_test_${Date.now()}_${Math.random().toString(36).slice(2)}.yaml`,
  );
  const yaml = defs.map((d) => jsonToYaml(d)).join("\n---\n");
  writeFileSync(path, yaml, "utf8");
  return path;
}

/** Minimal JSON-to-YAML converter sufficient for process definition objects. */
function jsonToYaml(value: unknown, indent = 0): string {
  const pad = "  ".repeat(indent);
  if (value === null || value === undefined) return "null";
  if (typeof value === "boolean") return String(value);
  if (typeof value === "number") return String(value);
  if (typeof value === "string") {
    if (
      /[:#{}[\],&*?|<>=!%@`]/.test(value) ||
      value === "" ||
      value === "true" ||
      value === "false" ||
      value === "null"
    ) {
      return JSON.stringify(value);
    }
    return value;
  }
  if (Array.isArray(value)) {
    if (value.length === 0) return "[]";
    return value
      .map((v) => {
        const rendered = jsonToYaml(v, indent + 1);
        const lines = rendered.split("\n");
        // First line uses "- " prefix; continuation lines keep their indentation.
        return [`${pad}- ${lines[0].trimStart()}`, ...lines.slice(1)].join(
          "\n",
        );
      })
      .join("\n");
  }
  if (typeof value === "object") {
    const entries = Object.entries(value as Record<string, unknown>);
    if (entries.length === 0) return "{}";
    return entries
      .map(([k, v]) => {
        if (
          v === null ||
          v === undefined ||
          (typeof v !== "object" && !Array.isArray(v))
        ) {
          return `${pad}${k}: ${jsonToYaml(v, indent + 1)}`;
        }
        if (Array.isArray(v) && (v as unknown[]).length === 0)
          return `${pad}${k}: []`;
        // The empty object is genroc's unknown type, so it is a real value a definition
        // carries. Block style would put the `{}` at column 0 and break the document.
        if (!Array.isArray(v) && Object.keys(v as object).length === 0)
          return `${pad}${k}: {}`;
        // Objects and non-empty arrays always use block (next-line) style.
        return `${pad}${k}:\n${jsonToYaml(v, indent + 1)}`;
      })
      .join("\n");
  }
  return String(value);
}
