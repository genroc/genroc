# Editors

`genctl lsp` is an ordinary language server, so any editor with an LSP client gets diagnostics,
hover, completion, go-to-definition and highlighting from it. Only VS Code needs an extension
([vscode/](vscode/)), and only because it also ships the server as WebAssembly for machines
without a `genctl`.

Everywhere else the setup is two facts: run `genctl lsp` over stdio, for `*.genroc.yaml`.

## Highlighting elsewhere

There is no genroc tree-sitter grammar and there does not need to be. A definition **is** YAML,
so your editor's own YAML highlighting applies; the genroc half arrives as LSP semantic tokens,
which mark the scalars that actually evaluate and lex what is inside them. A client that does
not implement semantic tokens ignores the capability and you keep plain YAML.

That split is deliberate rather than a shortcut: whether `"$: tick"` computes depends on the
slot holding it, and only the server knows slots — see internal/lsp/CLAUDE.md.

## Neovim

Semantic tokens are applied automatically once the server advertises them (0.9+).

```lua
vim.filetype.add({ pattern = { [".*%.genroc%.yaml"] = "yaml" } })
vim.api.nvim_create_autocmd("FileType", {
  pattern = "yaml",
  callback = function(args)
    if not vim.api.nvim_buf_get_name(args.buf):match("%.genroc%.yaml$") then return end
    vim.lsp.start({
      name = "genroc",
      cmd = { "genctl", "lsp" },
      root_dir = vim.fs.root(args.buf, { ".genroc", ".git" }),
    })
  end,
})
```

## Helix

`languages.toml`, as a separate language sharing YAML's grammar:

```toml
[[language]]
name = "genroc"
scope = "source.yaml"
file-types = [{ glob = "*.genroc.yaml" }]
grammar = "yaml"
language-servers = ["genroc"]

[language-server.genroc]
command = "genctl"
args = ["lsp"]
```

## Emacs

With eglot (Emacs 29+); `M-x eglot` in a definition.

```elisp
(add-to-list 'auto-mode-alist '("\\.genroc\\.yaml\\'" . yaml-mode))
(with-eval-after-load 'eglot
  (add-to-list 'eglot-server-programs '(yaml-mode . ("genctl" "lsp"))))
```

## Zed

`~/.config/zed/settings.json`, plus a `file_types` entry mapping the suffix to YAML.

```json
{
  "file_types": { "YAML": ["*.genroc.yaml"] },
  "lsp": { "genroc": { "binary": { "path": "genctl", "arguments": ["lsp"] } } }
}
```

## Checking it works

`genctl lsp` speaks LSP on stdio and prints nothing else — a stray line on stdout is a frame the
editor cannot parse, so silence is correct. If an editor shows nothing, check that `genctl` is on
its `PATH` (editors do not always inherit a shell's) before suspecting the server.
