package main

import (
	"flag"
	"fmt"
	"os"

	"genroc/internal/api"
)

func main() {
	out := flag.String("o", "openapi.json", `OpenAPI output path ("-" for stdout, "" to skip)`)
	schemaOut := flag.String("schema", "", `process-definition JSON Schema output path ("-" for stdout, "" to skip)`)
	flag.Parse()

	write(*out, api.Spec)
	write(*schemaOut, api.ProcessSchema)
}

// The generators are passed unevaluated so that skipping an output also skips building it.
func write(path string, build func() []byte) {
	switch path {
	case "":
		return
	case "-":
		os.Stdout.Write(build())
		return
	}

	data := build()
	if err := os.WriteFile(path, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", path, len(data))
}
