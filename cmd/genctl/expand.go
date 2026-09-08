package main

// Which files a command reads: `-f` and `definitions:` in the nearest `.genroc`, the globbing
// behind both, and the source documents they load. See cmd/genctl/CLAUDE.md.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"genroc/internal/defdoc"
	"genroc/internal/numeric"

	"github.com/bmatcuk/doublestar/v4"
)

// loadSourceDocs keeps the file each document came from: a directive's path resolves against
// it, and an error has to name it.
func loadSourceDocs(files []string) ([]sourceDoc, error) {
	var all []sourceDoc
	for _, path := range files {
		docs, err := readFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		all = append(all, docs...)
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no process definitions found in provided files")
	}
	return all, nil
}

// definitionPaths is the file list a command operates on. There are exactly two sources, and
// every command that reads definitions uses both: `-f`, and `definitions:` in the nearest
// `.genroc` when no `-f` was given. Files are never taken positionally -- `-f` accepts several
// values, so an unquoted `defs/*.yaml` needs no second syntax, and one rule covers apply,
// validate, types and compat alike.
//
// `-f` is LITERAL FIRST: a value naming an existing file is that file, never a pattern. Only a
// value naming nothing is globbed. That keeps `a[1].genroc.yaml` reachable when `a1.genroc.yaml`
// also exists and a pattern would have matched the wrong one, silently.
//
// `definitions:` entries are patterns outright: there is no filename to prefer, and a pattern
// matching nothing is a mistake worth reporting.
func definitionPaths(files []string) ([]string, error) {
	if len(files) == 0 {
		return expandPaths(defaultDefinitionPaths("."))
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

// expandPaths turns every argument into files. There is no path-versus-pattern distinction:
// a pattern with no metacharacters matches itself, so a plain filename is just the trivial
// case. `**` matches any depth (doublestar; the stdlib has none).
//
// A DIRECTORY is refused, pointing at the pattern that would do it: walking one implicitly
// hides both the depth and the filename filter, so it would silently differ from the pattern
// meant to replace it. Sorted, so a batch is deterministic.
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

// resolvedDefs loads, resolves every import directive, and hands back the plain documents
// the API takes. By this point no directive remains — the server has no resolver.
func resolvedDefs(files []string) ([]any, error) {
	defs, _, err := resolvedDefsLocated(files)
	return defs, err
}

// resolvedDefsLocated is resolvedDefs keeping the sources, for a caller that will report a
// failure and can point at the line it was written on.
func resolvedDefsLocated(files []string) ([]any, []sourceDoc, error) {
	docs, err := loadSourceDocs(files)
	if err != nil {
		return nil, nil, err
	}
	if _, err := resolveDocs(docs, "build"); err != nil {
		return nil, nil, err
	}
	out := make([]any, len(docs))
	for i, d := range docs {
		out[i] = d.doc
	}
	return out, docs, nil
}

// locate finds where a slot address was written among the loaded sources. Matching on the
// address rather than on the process name in the error prose means nothing has to parse a
// message: an address that resolves in exactly one document names that document.
//
// A missing REQUIRED field has no node of its own, so the search falls back to the shortest
// enclosing path that does — `tasks[0].id` points at the task that lacks an id.
func locate(docs []sourceDoc, address string) (string, defdoc.Span, bool) {
	// Exactly one document may claim the address: two that both resolve it are two processes
	// with the same task id, and pointing at either would be a guess.
	var file string
	var span defdoc.Span
	found := 0
	for _, d := range docs {
		if d.index == nil {
			continue
		}
		if sp, ok := d.index.Span(address); ok {
			file, span, found = d.file, sp, found+1
		}
	}
	if found == 1 {
		return file, span, true
	}
	if parent := defdoc.ParentPath(address); parent != "" {
		return locate(docs, parent)
	}
	return "", defdoc.Span{}, false
}

// readFile parses one source file. The parsed position index travels with each document so a
// failure the server reports by slot address can be printed as a line in this file.
func readFile(path string) ([]sourceDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".json" {
		// No index: JSON allows tabs where YAML does not, so it keeps its own decode and
		// gives up line numbers. A .json definition is generated far more often than written.
		var doc any
		if err := numeric.Decode(data, &doc); err != nil {
			return nil, fmt.Errorf("parse JSON: %w", err)
		}
		if arr, ok := doc.([]any); ok {
			out := make([]sourceDoc, len(arr))
			for i, d := range arr {
				out[i] = sourceDoc{doc: d, file: path}
			}
			return out, nil
		}
		return []sourceDoc{{doc: doc, file: path}}, nil
	}

	parsed, err := defdoc.ParseAll(data)
	if err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	docs := make([]sourceDoc, len(parsed))
	for i, d := range parsed {
		docs[i] = sourceDoc{doc: d.Value, file: path, index: d}
	}
	return docs, nil
}

// takeFileValues pulls `-f`/`--f` and every following argument up to the next flag into one
// list, and returns the rest. That makes `-f a b c` one flag with three values -- which is what
// an unquoted `-f defs/*.yaml` expands to -- while `-f a b --channel prod` still stops at the
// flag, so nothing after it is swallowed.
//
// Done here rather than by flag.Value because the stdlib gives a Value exactly one argument.
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

// fatalLocated prints a rejected apply as one line per failing slot, each pointing at the
// file and line it was written on — the payoff of the address travelling with the diagnostic.
// A failure with no per-field detail, or a slot no source claims, falls back to the message.
func fatalLocated(docs []sourceDoc, err error) {
	var se *serverError
	if !errors.As(err, &se) || len(se.Fields) == 0 {
		fatal("%v", err)
	}
	for _, f := range se.Fields {
		file, span, ok := locate(docs, f.Field)
		if !ok {
			fmt.Fprintf(os.Stderr, "genctl: %s: %s\n", f.Field, f.Message)
			continue
		}
		fmt.Fprintf(os.Stderr, "genctl: %s:%d:%d: %s\n", file, span.Value.Line, span.Value.Col, f.Message)
	}
	os.Exit(1)
}
