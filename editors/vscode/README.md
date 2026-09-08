# genroc for VS Code

Diagnostics, hover, completion and go-to-definition for `*.genroc.yaml`.

The extension is a launcher: it starts `genctl lsp` and speaks LSP to it. Every answer is the
server's own analysis — the same `Validate` and inference an `apply` runs — so the editor and
the CLI cannot disagree about what is wrong.

## Requires

`genctl` on your `PATH` (or set `genroc.server.path`). Check with `genctl lsp --help`.

## What it does

| | |
|---|---|
| **Diagnostics** | every broken slot, not just the first, underlined on the field that is wrong |
| **Hover** | the type an expression infers to, a slot's type, and the scope it is written in |
| **Completion** | scope members inside `$:` / `${ }`; legal keys elsewhere, discriminated — a `fetch` action offers fetch's keys, not the union of six |
| **Go to definition** | `goto: "$task-id"` jumps to that task |

Completion and hover work on a document that does not parse yet, which is the state a file is
in while you are typing in it.

## `yaml-language-server`

Turn it off for these files, or the two servers double-report:

```jsonc
"yaml.schemas": {},                 // or drop the $schema comment from your definitions
"[genroc]": { "editor.defaultFormatter": null }
```

The `# yaml-language-server: $schema=` comment stays useful for anyone without this extension.

## Building it

```sh
cd editors/vscode
npm install
npm run compile
npm run package     # produces genroc-<version>.vsix
code --install-extension genroc-0.1.0.vsix
```
