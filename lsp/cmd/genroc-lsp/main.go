// genroc-lsp speaks LSP over stdio for `*.genroc.yaml`. specs/language-server.md.
package main

import (
	"os"

	"genroc/lsp"
)

// version is set by the build; unset outside one, which an editor reports harmlessly.
var version = "dev"

func main() {
	os.Exit(lsp.New(os.Stdin, os.Stdout, version).Run())
}
