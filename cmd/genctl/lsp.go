package main

// `genctl lsp` is the language server: the same `Validate` and `validation.Check` every other
// command runs, spoken over stdio to an editor. specs/language-server.md.

import (
	"os"

	"genroc/internal/lsp"
)

func runLSPCmd(args []string) {
	if hasHelpArg(args) {
		helpFor("lsp")
		return
	}
	for _, a := range args {
		// vscode-languageclient appends `--stdio` on its own; refusing it would refuse the client.
		if a == "--stdio" || a == "-stdio" {
			continue
		}
		fatal("lsp takes no arguments except --stdio; it speaks LSP over stdin and stdout")
	}
	blockStdio()
	// stdout is the protocol's from here: nothing else may print to it.
	os.Exit(lsp.New(os.Stdin, os.Stdout, versionString()).Run())
}
