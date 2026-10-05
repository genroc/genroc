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
	httpRef := flag.String("http-reference", "", `directory to write the HTTP endpoint reference pages into ("" to skip)`)
	defRef := flag.String("definition-reference", "", `directory to write the definition-language reference pages into ("" to skip)`)
	errRef := flag.Bool("error-reference", false,
		"write the error-code pages into the definition and HTTP reference directories (needs both)")
	configRef := flag.String("config-reference", "", `directory to write the configuration reference pages into ("" to skip)`)
	statusTable := flag.String("status-table", "", `file to write the bare instance-status table into, for a guide to import ("" to skip)`)
	serverRef := flag.String("server-reference", "", `directory to write the server flag reference page into ("" to skip)`)
	genctl := flag.String("genctl", "./genctl", "the genctl binary the CLI reference is read from")
	genroc := flag.String("genroc", "./genroc", "the genroc binary the server reference is read from")
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
	if *serverRef != "" {
		if err := writeServerReference(*serverRef, *genroc); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}
	if *httpRef != "" {
		if err := writeHTTPReference(*httpRef); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}
	if *defRef != "" {
		if err := writeDefinitionReference(*defRef); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}
	if *errRef {
		if *defRef == "" || *httpRef == "" {
			fmt.Fprintln(os.Stderr, "error: --error-reference needs --definition-reference and --http-reference")
			os.Exit(1)
		}
		if err := writeErrorReference(*defRef, *httpRef); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}
	if *configRef != "" {
		if err := writeConfigReference(*configRef); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}
	if *statusTable != "" {
		if err := writeStatusTable(*statusTable); err != nil {
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
