package lsp

// The PATH a `$<resolver>:` directive names: offering one, and following one. Resolution never
// treats the argument as a path, so this is the editor GUESSING, shell-style — nothing here
// changes what a resolver accepts. specs/source-resolution.md.

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"genroc/internal/defdoc"
	"genroc/internal/sources"
)

// directiveArgRe: a name starting with a LETTER and a space after the colon — defdoc.Directive's
// two rules, which keep `$:`, `${` and a `$a:b` routing target out.
var directiveArgRe = regexp.MustCompile(`\$([a-zA-Z][a-zA-Z0-9_-]*):[ \t]+`)

// directiveArgAt reads the directive argument the cursor sits in: the resolver's name, the text
// typed so far, and the 1-based byte column that text starts at.
func directiveArgAt(src string, col int) (name, typed string, from int, ok bool) {
	for _, m := range directiveArgRe.FindAllStringSubmatchIndex(src, -1) {
		start := m[1] // just past `$name:` and its spaces
		end := argEnd(src)
		if end < start || col-1 < start || col-1 > end {
			continue
		}
		return src[m[2]:m[3]], src[start : col-1], start + 1, true
	}
	return "", "", 0, false
}

// argEnd is where a directive's argument stops on the raw line: before trailing space, and
// before the quote that closes the scalar it is written in.
func argEnd(src string) int {
	end := len(strings.TrimRight(src, " \t"))
	if end > 0 && (src[end-1] == '"' || src[end-1] == '\'') {
		end--
	}
	return end
}

// looksLikePath: `/`, `./` or `../` begins a path; a bare name may be a package or a URL. A lone
// `.` is deliberately not enough — it is a trigger character, so every typed dot would list a
// directory.
func looksLikePath(typed string) bool {
	return strings.HasPrefix(typed, "/") ||
		strings.HasPrefix(typed, "./") ||
		strings.HasPrefix(typed, "../")
}

// offersPaths adds an EMPTY argument to looksLikePath: there is no meaning yet to invent.
func offersPaths(typed string) bool {
	return typed == "" || looksLikePath(typed)
}

// directivePathValues offers the files and folders that could continue the path being typed.
func directivePathValues(text, file string, line, col int) ([]completionItem, bool) {
	src := lineAt(text, line)
	name, typed, from, ok := directiveArgAt(src, col)
	if !ok || !offersPaths(typed) {
		return nil, false
	}
	dir, base := path.Split(typed)
	// Offer `./name`, never a bare name: a resolver may read that as something other than a path.
	prefix := ""
	if dir == "" {
		prefix = "./"
	}
	entries, err := os.ReadDir(resolveDir(dir, file))
	if err != nil {
		// A directory that does not exist yet is a path mid-typing, not a question with a
		// different answer — claim the position so the key list is not offered instead.
		return []completionItem{}, true
	}
	suffixes, known := suffixesFor(file, name)
	replaceFrom := from + len(dir)

	out := []completionItem{}
	for _, e := range entries {
		// Dotfiles are hidden until one is asked for, which is what a shell does.
		if strings.HasPrefix(e.Name(), ".") && !strings.HasPrefix(base, ".") {
			continue
		}
		if e.IsDir() {
			out = append(out, completionItem{
				Label: e.Name() + "/", Kind: kindFolder, Detail: "folder",
				// No trailing space and no colon: a folder is a step, not an answer.
				insert: prefix + e.Name() + "/", SortText: "0" + e.Name(), replaceFrom: replaceFrom,
			})
			continue
		}
		if known && !acceptsSuffix(suffixes, e.Name()) {
			continue
		}
		out = append(out, completionItem{
			Label: e.Name(), Kind: kindFile, Detail: name,
			insert: prefix + e.Name(), SortText: "1" + e.Name(), replaceFrom: replaceFrom,
		})
	}
	return out, true
}

// suffixesFor reads the accepted suffixes from the project's `.genroc`. A name no resolver carries
// is left UNFILTERED: an empty list mid-typing reads as a server that does not work.
func suffixesFor(file, name string) ([]string, bool) {
	cfg, err := sources.FindProjectConfig(filepath.Dir(file))
	if err != nil {
		return nil, false
	}
	suffixes, known := sources.Suffixes(cfg, name)
	return suffixes, known && len(suffixes) > 0
}

func acceptsSuffix(suffixes []string, name string) bool {
	lower := strings.ToLower(name)
	for _, s := range suffixes {
		if strings.HasSuffix(lower, strings.ToLower(s)) {
			return true
		}
	}
	return false
}

// resolveDir: relative is against the FILE holding the directive, never the workspace root —
// resolveProcessDirective's rule, so the editor looks where an apply does.
func resolveDir(dir, file string) string {
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	return filepath.Join(filepath.Dir(file), dir)
}

// directiveFileAt returns the file a directive under the cursor names, for go-to-definition.
// The whole leaf counts, not just its argument: a reader points at the path and means the file.
func directiveFileAt(doc *defdoc.Doc, docPath, file string) (string, bool) {
	v, ok := doc.ValueAt(docPath)
	if !ok {
		return "", false
	}
	leaf, ok := v.(string)
	if !ok {
		return "", false
	}
	_, argument, ok := defdoc.Directive(leaf)
	if !ok || !looksLikePath(argument) {
		return "", false
	}
	target := resolveDir(argument, file)
	if _, err := os.Stat(target); err != nil {
		return "", false
	}
	return target, true
}
