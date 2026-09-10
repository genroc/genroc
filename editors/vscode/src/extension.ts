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

// The extension is a launcher. Every answer a user sees — diagnostics, hover, completion,
// go-to-definition — is `genctl lsp`'s, which is the server's own analysis rather than a
// second implementation of it. specs/language-server.md.

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

  // A genctl on PATH is preferred over the bundled one, so an OLD genctl silently costs
  // whatever it does not implement. Highlighting is the one a reader notices and cannot
  // explain — it simply looks like the extension does nothing.
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

// No `transport: TransportKind.stdio`. It is the default, and naming it makes the client APPEND
// `--stdio` to argv — which this extension would then depend on the binary accepting. Asking for
// a flag we do not need is how a genctl that predates it turns into five restarts and a disposed
// connection. The server accepts `--stdio` for clients that do send it.
function installedServer(command: string): Executable {
  return { command, args: ["lsp"] };
}

// wasmServer is the same server compiled to WebAssembly, shipped in the extension for a machine
// with no genctl on it. One module covers every platform, which is what keeps this a single
// universal .vsix; it is the FALLBACK because it is several times slower than the binary and
// carries the version the extension shipped with rather than the one that applies.
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
