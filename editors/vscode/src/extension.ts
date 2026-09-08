import { execFile } from "node:child_process";
import { promisify } from "node:util";
import * as vscode from "vscode";
import {
  LanguageClient,
  LanguageClientOptions,
  ServerOptions,
  TransportKind,
} from "vscode-languageclient/node";

// The extension is a launcher. Every answer a user sees — diagnostics, hover, completion,
// go-to-definition — is `genctl lsp`'s, which is the server's own analysis rather than a
// second implementation of it. specs/language-server.md.

let client: LanguageClient | undefined;

export async function activate(context: vscode.ExtensionContext) {
  const config = vscode.workspace.getConfiguration("genroc");
  if (!config.get<boolean>("server.enabled", true)) return;

  const command = config.get<string>("server.path", "genctl");
  const version = await probe(command);
  if (!version) {
    // A missing binary is the one failure a user can act on, so it is said once and plainly
    // rather than left as an extension that silently does nothing.
    vscode.window.showWarningMessage(
      `genroc: could not run \`${command} lsp\`. Install genctl, or set genroc.server.path.`,
    );
    return;
  }

  const server: ServerOptions = {
    run: { command, args: ["lsp"], transport: TransportKind.stdio },
    debug: { command, args: ["lsp"], transport: TransportKind.stdio },
  };

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

// probe runs `genctl -v` rather than starting the server, so a missing or wrong binary is
// found before the client is wired up and starts reporting restarts.
async function probe(command: string): Promise<string | undefined> {
  try {
    const { stdout } = await promisify(execFile)(command, ["-v"], { timeout: 5000 });
    return stdout.trim();
  } catch {
    return undefined;
  }
}
