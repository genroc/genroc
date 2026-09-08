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
	if len(args) > 0 {
		fatal("lsp takes no arguments; it speaks LSP over stdin and stdout")
	}
	// stdout is the protocol's channel from here: anything else written to it is a frame the
	// editor cannot parse, which is why nothing in this command prints.
	os.Exit(lsp.New(os.Stdin, os.Stdout, versionString()).Run())
}
