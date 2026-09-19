package lsp

// The PATH a `$<resolver>:` directive names: offering one, and following one.
//
// Resolution never treats the argument as a path — `findSites` passes it verbatim, because
// genroc does not know that a resolver's argument is a file, let alone which one. So this is the
// editor GUESSING, shell-style: an argument that begins `/` or `.` is one someone is typing a
// path into, and nothing else is offered. Nothing here changes what a resolver accepts.
// specs/source-resolution.md.

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"genroc/internal/defdoc"
	"genroc/internal/sources"
)

// directiveArgRe finds `$name:` on a raw line. The name must start with a LETTER, and the colon
// must be followed by a space — the same two rules defdoc.Directive follows, so the editor and
// the resolver agree on what a directive is. The letter keeps `$:` (an expression) and `${` (an
// interpolation) out; the space keeps a routing target out, `$a:b` being a task id and not a
// resolver call. The cost is that paths are offered once the space is typed rather than on the
// colon, which is one keystroke and the price of not offering them over a `goto`.
var directiveArgRe = regexp.MustCompile(`\$([a-zA-Z][a-zA-Z0-9_-]*):[ \t]+`)

// directiveArgAt reads the directive argument the cursor sits in: the resolver's name, the text
// typed so far, and the 1-based byte column that text starts at.
//
// It works on the RAW line, like every other completion source here, because the document a
// reader is typing into does not parse.
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

// looksLikePath is the shell-style guess: `/`, `./` or `../` begins a path. A bare NAME does
// not — a resolver's argument may be a package, a URL or a key, and offering files for
// `$import: lodash` would invent a meaning the resolver never gave it.
//
// A lone `.` is deliberately NOT enough, though it is a prefix of two spellings that are. `.`
// is a trigger character (a member list needs it), so accepting it put a directory listing on
// screen the instant anyone typed a dot — dotfiles and all, unasked. One more keystroke says
// which of the two a reader meant, and until then there is nothing worth guessing at.
func looksLikePath(typed string) bool {
	return strings.HasPrefix(typed, "/") ||
		strings.HasPrefix(typed, "./") ||
		strings.HasPrefix(typed, "../")
}

// offersPaths adds the one case looksLikePath cannot cover: an EMPTY argument, where there is
// no meaning yet to invent and a reader has nothing to go on. Without it the first keystroke
// has to be guessed blind — `$process: ` answered with nothing at all, which is where this
// started. Type a name and the offer stops; type `.` or `/` and it continues.
func offersPaths(typed string) bool {
	return typed == "" || looksLikePath(typed)
}

// directivePathValues offers the files and folders that could continue the path being typed.
// The file's own directory is what a relative path resolves against, which is the rule the
// resolver follows when it joins the two.
func directivePathValues(text, file string, line, col int) ([]completionItem, bool) {
	src := lineAt(text, line)
	name, typed, from, ok := directiveArgAt(src, col)
	if !ok || !offersPaths(typed) {
		return nil, false
	}
	dir, base := path.Split(typed)
	// A relative path is written `./name`. Explicit is clearer than a bare name, and a bare
	// name is also the one spelling a resolver may read as something that is not a path at all
	// — so what is offered must not produce one.
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

// suffixesFor reads the accepted suffixes out of the project's `.genroc`. A name no resolver
// carries is left UNFILTERED rather than emptied: the registry is the reader's to fix, and an
// empty list at the moment they are typing reads as a server that does not work.
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

// resolveDir is where a directive's `dir` part points. Relative is against the FILE holding the
// directive, never the workspace root — that is the rule resolveProcessDirective joins by, and
// a second answer here would send the editor somewhere an apply never looks.
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
