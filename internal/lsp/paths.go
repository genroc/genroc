package lsp

// The PATH a `$<resolver>:` directive names: offering one, and following one. Resolution never
// treats the argument as a path, so this is the editor GUESSING, shell-style — nothing here
// changes what a resolver accepts. specs/source-resolution.md.

import (
	"errors"
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

// argWord is the word of a directive's argument the cursor is in, as typed up to the cursor.
type argWord struct {
	name  string // the resolver
	index int    // which word: `ext` filters only the first
	value string // after quote removal
	raw   string // as written, quotes included
	col   int    // 1-based byte column where raw starts
	open  bool   // inside a ' that is not closed yet
	// yamlSingle: the directive is a YAML single-quoted scalar, where a ' of ours is written ''.
	yamlSingle bool
}

// directiveWordAt splits the argument up to the cursor with defdoc.SplitArgs, the rule genctl
// splits by, so the word offered is the word a resolver will be given.
func directiveWordAt(src string, col int) (argWord, bool) {
	for _, m := range directiveArgRe.FindAllStringSubmatchIndex(src, -1) {
		start := m[1] // just past `$name:` and its spaces
		end := argEnd(src)
		if end < start || col-1 < start || col-1 > end {
			continue
		}
		typed := src[start : col-1]
		words, err := defdoc.SplitArgs(typed)
		var argErr *defdoc.ArgError
		if err != nil && !(errors.As(err, &argErr) && argErr.Unterminated) {
			return argWord{}, false
		}
		w := argWord{name: src[m[2]:m[3]], open: err != nil, yamlSingle: m[0] > 0 && src[m[0]-1] == '\''}
		if !w.open && (len(words) == 0 || words[len(words)-1].End < len(typed)) {
			// The cursor is past a blank: a new word starts here.
			w.index, w.col = len(words), col
			return w, true
		}
		last := words[len(words)-1]
		w.index, w.value, w.raw, w.col = len(words)-1, last.Value, typed[last.Start:], start+last.Start+1
		return w, true
	}
	return argWord{}, false
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

// offersPaths adds an EMPTY first word to looksLikePath: there is no meaning yet to invent. A
// later empty word is a parameter as often as a path, so it waits for one.
func offersPaths(w argWord) bool {
	return (w.index == 0 && w.value == "") || looksLikePath(w.value)
}

// needsQuote: what SplitArgs would split or refuse unquoted.
func needsQuote(name string) bool { return strings.ContainsAny(name, " \t\"\\") }

// directivePathValues offers the files and folders that could continue the path being typed.
func directivePathValues(text, file string, line, col int) ([]completionItem, bool) {
	src := lineAt(text, line)
	w, ok := directiveWordAt(src, col)
	if !ok || !offersPaths(w) || (w.yamlSingle && strings.Contains(w.raw, "'")) {
		return nil, false
	}
	dir, base := path.Split(w.value)
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
	var suffixes []string
	known := false
	if w.index == 0 {
		suffixes, known = suffixesFor(file, w.name)
	}
	// The name part begins after the last `/`, which is literal quoted or not, and after a quote
	// opening there; the range starts there so the editor filters by the name alone.
	nameAt := 0
	if i := strings.LastIndexByte(w.raw, '/'); i >= 0 {
		nameAt = i + 1
	}
	if nameAt < len(w.raw) && w.raw[nameAt] == '\'' {
		nameAt++
	}
	replaceFrom := w.col + nameAt
	closed := col-1 < len(src) && src[col-1] == '\''

	out := []completionItem{}
	for _, e := range entries {
		// Dotfiles are hidden until one is asked for, which is what a shell does.
		if strings.HasPrefix(e.Name(), ".") && !strings.HasPrefix(base, ".") {
			continue
		}
		// A ' cannot be written inside our quotes, nor any quote inside YAML's single ones.
		if strings.Contains(e.Name(), "'") || (w.yamlSingle && needsQuote(e.Name())) {
			continue
		}
		if e.IsDir() {
			out = append(out, completionItem{
				Label: e.Name() + "/", Kind: kindFolder, Detail: "folder",
				// No trailing space and no colon: a folder is a step, not an answer.
				insert: prefix + quoted(e.Name(), w.open, true, closed) + "/", SortText: "0" + e.Name(),
				replaceFrom: replaceFrom,
			})
			continue
		}
		if known && !acceptsSuffix(suffixes, e.Name()) {
			continue
		}
		out = append(out, completionItem{
			Label: e.Name(), Kind: kindFile, Detail: w.name,
			insert: prefix + quoted(e.Name(), w.open, false, closed), SortText: "1" + e.Name(),
			replaceFrom: replaceFrom,
		})
	}
	return out, true
}

// quoted writes a name so SplitArgs reads it back: inside an open quote as it is, closed after a
// file unless the quote is closed already; outside one, quoted only if it must be. A folder stays
// open, since the path goes on.
func quoted(name string, open, folder, closed bool) string {
	switch {
	case open && !folder && !closed:
		return name + "'"
	case open || !needsQuote(name):
		return name
	default:
		return "'" + name + "'"
	}
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
	if !ok {
		return "", false
	}
	args, err := defdoc.ArgValues(argument)
	if err != nil || len(args) == 0 || !looksLikePath(args[0]) {
		return "", false
	}
	target := resolveDir(args[0], file)
	if _, err := os.Stat(target); err != nil {
		return "", false
	}
	return target, true
}
