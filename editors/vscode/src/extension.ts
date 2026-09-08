import { execFile } from "node:child_process";
import { promisify } from "node:util";
import * as vscode from "vscode";
import {
  LanguageClient,
  LanguageClientOptions,
  ServerOptions,
} from "vscode-languageclient/node";

// The extension is a launcher. Every answer a user sees — diagnostics, hover, completion,
// go-to-definition — is `genctl lsp`'s, which is the server's own analysis rather than a
// second implementation of it. specs/language-server.md.

let client: LanguageClient | undefined;

export async function activate(context: vscode.ExtensionContext) {
  const config = vscode.workspace.getConfiguration("genroc");
  if (!config.get<boolean>("server.enabled", true)) return;

  const command = config.get<string>("server.path", "genctl");
  const supported = await hasLspCommand(command);
  if (!supported) {
    // Said once and plainly. Without this the client starts, `genctl` prints its usage to
    // stdout instead of a frame, and the user sees five restarts and a disposed connection —
    // which is what a binary predating `genctl lsp` did before this check existed.
    const version = await run(command, ["-v"]);
    vscode.window.showWarningMessage(
      version === undefined
        ? `genroc: could not run \`${command}\`. Install genctl, or set genroc.server.path.`
        : `genroc: \`${command}\` (${version}) has no \`lsp\` command. Update genctl.`,
    );
    return;
  }

  // No `transport: TransportKind.stdio`. It is the default, and naming it makes the client
  // APPEND `--stdio` to argv — which this extension would then depend on the binary accepting.
  // Asking for a flag we do not need is how a genctl that predates it turns into five restarts
  // and a disposed connection. The server accepts `--stdio` for clients that do send it.
  const executable = { command, args: ["lsp"] };
  const server: ServerOptions = { run: executable, debug: executable };

  const client_options: LanguageClientOptions = {
    // Both ids: a workspace that has not adopted the `genroc` language id still has these
    // files open as plain YAML, and the server decides for itself which URIs it answers for.
    documentSelector: [
      { scheme: "file", language: "genroc" },
      { scheme: "file", language: "yaml", pattern: "**/*.genroc.{yaml,yml}" },
    ],
    outputChannelName: "genroc",
  };

  client = new LanguageClient("genroc", "genroc", server, client_options);
  await client.start();
  context.subscriptions.push(client);
}

export function deactivate(): Thenable<void> | undefined {
  return client?.stop();
}

// hasLspCommand asks for the subcommand's own help, which exits 0 only on a binary that has
// it. `-v` is not enough: it succeeds on every genctl ever built, including the ones that
// answer `genctl lsp` with a usage dump and exit 1.
async function hasLspCommand(command: string): Promise<boolean> {
  return (await run(command, ["lsp", "-h"])) !== undefined;
}

async function run(command: string, args: string[]): Promise<string | undefined> {
  try {
    const { stdout } = await promisify(execFile)(command, args, { timeout: 5000 });
    return stdout.trim();
  } catch {
    return undefined;
  }
}
