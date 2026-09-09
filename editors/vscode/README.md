# genroc for VS Code

Diagnostics, hover, completion and go-to-definition for `*.genroc.yaml`.

The extension is a launcher: it starts `genctl lsp` and speaks LSP to it. Every answer is the
server's own analysis — the same `Validate` and inference an `apply` runs — so the editor and
the CLI cannot disagree about what is wrong.

## Requires

Nothing. The extension carries the server as WebAssembly and runs that where the machine has no
genctl on it.

A `genctl` on your `PATH` is preferred and picked up automatically: it is several times faster,
and it is the version that will `apply` — the bundled one is whatever this extension shipped
with. Check yours with `genctl lsp --help`, name a different one with `genroc.server.path`, or
turn the fallback off with `genroc.server.bundled: false`.

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

## Installing it

From the Marketplace, or — to track `main` — the `genroc-edge.vsix` attached to the
[edge release](https://github.com/genroc/genroc/releases/tag/edge):

```sh
code --install-extension genroc-edge.vsix
```

## Building it

```sh
cd editors/vscode
npm install
npm run compile
npm run package     # produces genroc-<version>.vsix
code --install-extension genroc-0.0.0.vsix
```

The version in this directory is a placeholder: a release takes its version from the git tag,
the way the npm package does.
