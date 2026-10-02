import { execFile } from "node:child_process";
import { existsSync } from "node:fs";
import { promisify } from "node:util";
import * as vscode from "vscode";
import {
  Executable,
  LanguageClient,
  LanguageClientOptions,
  ServerOptions,
} from "vscode-languageclient/node";

// The extension is a launcher: every answer is `genctl lsp`'s, never a second implementation.
// specs/language-server.md.

let client: LanguageClient | undefined;

export async function activate(context: vscode.ExtensionContext) {
  const config = vscode.workspace.getConfiguration("genroc");
  if (!config.get<boolean>("server.enabled", true)) return;

  const command = config.get<string>("server.path", "genctl");
  const bundled = config.get<boolean>("server.bundled", true);
  let fallback = false;
  let executable = installedServer(command);

  if (!(await hasLspCommand(command))) {
    const wasm = bundled && !isSet(config, "server.path") ? wasmServer(context) : undefined;
    if (wasm === undefined) {
      // Without this the client starts, `genctl` prints usage instead of a frame, and the user
      // sees five restarts and a disposed connection.
      const version = await run(command, ["-v"]);
      vscode.window.showWarningMessage(
        version === undefined
          ? `genroc: could not run \`${command}\`. Install genctl, or set genroc.server.path.`
          : `genroc: \`${command}\` (${version}) has no \`lsp\` command. Update genctl.`,
      );
      return;
    }
    executable = wasm;
    fallback = true;
  }

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

  // A genctl on PATH beats the bundled one, so an OLD one silently lacks features; missing
  // highlighting is the one a reader cannot explain.
  if (!client.initializeResult?.capabilities.semanticTokensProvider) {
    client.outputChannel.appendLine(
      "this genctl has no semanticTokens support, so expressions are not highlighted. Update genctl.",
    );
  }
  if (fallback) {
    // The channel, not a notification: it changes nothing a reader has to act on, and it is
    // the first thing to know when an answer here disagrees with the genctl they install later.
    client.outputChannel.appendLine(
      "no genctl on PATH — running the bundled genctl.wasm. Install genctl for the native server.",
    );
  }
}

export function deactivate(): Thenable<void> | undefined {
  return client?.stop();
}

// No `transport: TransportKind.stdio`: it is the default, and naming it APPENDS `--stdio`, which a
// genctl predating it answers with five restarts and a disposed connection.
function installedServer(command: string): Executable {
  return { command, args: ["lsp"] };
}

// wasmServer is the FALLBACK: slower than the binary, and the version the extension shipped
// rather than the one that applies. specs/language-server.md §4.
function wasmServer(context: vscode.ExtensionContext): Executable | undefined {
  const module = vscode.Uri.joinPath(context.extensionUri, "bin", "genctl.wasm").fsPath;
  if (!existsSync(module)) return undefined;
  const loader = vscode.Uri.joinPath(context.extensionUri, "wasi.mjs").fsPath;
  const roots = (vscode.workspace.workspaceFolders ?? []).map((folder) => folder.uri.fsPath);
  return {
    // VS Code's own Electron, run as plain Node — where node:wasi lives. Nothing to install,
    // and no runtime that can be a different version from the editor's.
    command: process.execPath,
    args: ["--no-warnings", loader, module, ...roots],
    options: { env: { ...process.env, ELECTRON_RUN_AS_NODE: "1" } },
  };
}

// isSet reports whether a setting was written somewhere, as opposed to carrying its default. An
// author who NAMED a genctl is told it is broken rather than quietly given a different server.
function isSet(config: vscode.WorkspaceConfiguration, section: string): boolean {
  const value = config.inspect<string>(section);
  return (
    value?.globalValue !== undefined ||
    value?.workspaceValue !== undefined ||
    value?.workspaceFolderValue !== undefined
  );
}

// Not `-v`: it succeeds on every genctl, including ones that answer `genctl lsp` with a usage
// dump and exit 1.
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
