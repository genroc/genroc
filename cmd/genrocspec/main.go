package main

import (
	"flag"
	"fmt"
	"os"

	"genroc/internal/api"
	"genroc/internal/defschema"
)

func main() {
	out := flag.String("o", "openapi.json", `OpenAPI output path ("-" for stdout, "" to skip)`)
	schemaOut := flag.String("schema", "", `process-definition JSON Schema output path ("-" for stdout, "" to skip)`)
	configOut := flag.String("config-schema", "", `.genroc project-config JSON Schema output path ("-" for stdout, "" to skip)`)
	cliRef := flag.String("cli-reference", "", `directory to write the genctl reference pages into ("" to skip)`)
	genctl := flag.String("genctl", "./genctl", "the genctl binary the CLI reference is read from")
	flag.Parse()

	write(*out, api.Spec)
	write(*schemaOut, api.ProcessSchema)
	write(*configOut, defschema.Config)

	if *cliRef != "" {
		if err := writeCLIReference(*cliRef, *genctl); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}
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
