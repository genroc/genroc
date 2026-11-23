package main

// Which files a command reads: `-f` and `definitions:` in the nearest `.genroc`, the globbing
// behind both, and the source documents they load. See cmd/genctl/CLAUDE.md.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"genroc/internal/lsp"
	"genroc/internal/model"
	"genroc/internal/numeric"
	"genroc/internal/sources"

	"github.com/bmatcuk/doublestar/v4"
)

// definitionPaths is `-f`, else `definitions:` in the nearest `.genroc` -- never positionals.
// Rules: cmd/genctl/CLAUDE.md "Which files a command reads".
func definitionPaths(files []string) ([]string, error) {
	if len(files) == 0 {
		return expandPaths(sources.DefaultDefinitionPaths("."))
	}
	return expandFileFlags(files)
}

// expandFileFlags resolves `-f` values: an existing path wins over any pattern reading of it.
func expandFileFlags(files []string) ([]string, error) {
	var out []string
	for _, f := range files {
		if _, err := os.Stat(f); err == nil {
			out = append(out, f)
			continue
		}
		matched, err := expandPaths([]string{f})
		if err != nil {
			return nil, err
		}
		out = append(out, matched...)
	}
	return out, nil
}

// expandPaths refuses a DIRECTORY rather than walking it (see cmd/genctl/CLAUDE.md). Sorted, so
// a batch is deterministic.
func expandPaths(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		matches, err := doublestar.FilepathGlob(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if len(matches) == 0 {
			// A real file whose NAME contains `[`, `*` or `{` matches no pattern, including
			// its own. Falling back to the literal keeps such a file reachable unescaped.
			if _, statErr := os.Stat(p); statErr == nil {
				matches = []string{p}
			} else if strings.ContainsAny(p, "*?[{") {
				return nil, fmt.Errorf("%s: matched no files", p)
			} else {
				// Wording only, not a second code path: "matched no files" reads as a broken
				// pattern when what happened is a mistyped name.
				return nil, fmt.Errorf("%s: no such file", p)
			}
		}
		sort.Strings(matches)
		for _, m := range matches {
			if info, err := os.Stat(m); err == nil && info.IsDir() {
				return nil, fmt.Errorf("%s is a directory; give a pattern, e.g. %s",
					m, filepath.Join(m, "**", "*.genroc.yaml"))
			}
		}
		out = append(out, matches...)
	}
	return out, nil
}

// resolvedDefsLocated leaves no import directive behind, since the server has no resolver, and
// exits on a document that does not decode: the server could name only the definition, not the key.
func resolvedDefsLocated(files []string) ([]any, []sources.Doc, error) {
	docs, err := sources.LoadDocs(files)
	if err != nil {
		return nil, nil, err
	}
	if err := sources.Resolve(docs); err != nil {
		return nil, nil, err
	}
	refuseUndecodable(docs)
	out := make([]any, len(docs))
	for i, d := range docs {
		out[i] = d.Value
	}
	return out, docs, nil
}

func refuseUndecodable(docs []sources.Doc) {
	failed := false
	for _, d := range docs {
		raw, err := json.Marshal(d.Value)
		if err != nil {
			fatal("%s: %v", d.File, err)
		}
		var def model.ProcessDefinition
		if err := numeric.DecodeStrict(raw, &def); err != nil {
			failed = true
			if d.Index == nil {
				fmt.Fprintf(os.Stderr, "genctl: %s: %v\n", d.File, err)
				continue
			}
			r, msg := lsp.DecodeRefusal(d.Index, err)
			fmt.Fprintf(os.Stderr, "genctl: %s:%d:%d: %s\n", d.File, r.Line, r.Col, msg)
		}
	}
	if failed {
		os.Exit(1)
	}
}

// takeFileValues gives `-f` every argument up to the next flag. Not a flag.Value: the stdlib hands
// a Value exactly one argument.
func takeFileValues(args []string) (files, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-f" || a == "--f":
			for i++; i < len(args) && !strings.HasPrefix(args[i], "-"); i++ {
				files = append(files, args[i])
			}
			i-- // the loop's own i++ steps onto the flag that stopped us
		case strings.HasPrefix(a, "-f=") || strings.HasPrefix(a, "--f="):
			files = append(files, a[strings.Index(a, "=")+1:])
		default:
			rest = append(rest, a)
		}
	}
	return files, rest
}

// looksLikePath is deliberately crude: a process name has no separator and no definition
// suffix, so anything carrying one was meant as a file.
func looksLikePath(s string) bool {
	if strings.ContainsAny(s, "/\\") {
		return true
	}
	for _, ext := range []string{".yaml", ".yml", ".json"} {
		if strings.HasSuffix(s, ext) {
			return true
		}
	}
	return false
}

// fatalLocated prints one file:line per failing slot, falling back to the message for a failure
// with no per-field detail. A field's definition index is into docs, which are the request's order.
func fatalLocated(docs []sources.Doc, err error) {
	var se *serverError
	if !errors.As(err, &se) || len(se.Fields) == 0 || len(docs) == 0 {
		fatal("%v", err)
	}
	for _, f := range se.Fields {
		if f.DefinitionIndex < 0 || f.DefinitionIndex >= len(docs) {
			fmt.Fprintf(os.Stderr, "genctl: %s: %s\n", f.Field, f.Message)
			continue
		}
		doc := docs[f.DefinitionIndex]
		if doc.Index == nil {
			fmt.Fprintf(os.Stderr, "genctl: %s: %s\n", doc.File, f.Message)
			continue
		}
		span, _ := doc.Index.Locate(f.Field)
		fmt.Fprintf(os.Stderr, "genctl: %s:%d:%d: %s\n", doc.File, span.Value.Line, span.Value.Col, f.Message)
	}
	os.Exit(1)
}
