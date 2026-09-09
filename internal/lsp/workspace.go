package lsp

// Finding a process by name across the workspace.
//
// The folders come from `initialize`, not from `.genroc`: an editor already knows what is open,
// and `definitions:` answers a different question — which files an `apply` deploys, not which
// exist. specs/language-server.md §7.

import (
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"genroc/internal/defdoc"
)

// skipDirs are trees a definition is never in and that are big enough to be worth not walking.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "build": true, "vendor": true,
}

// maxScanned caps a walk so a workspace rooted somewhere enormous cannot hang an editor. A
// project with more definitions than this has outgrown a whole-workspace scan, not this number.
const maxScanned = 2000

// findProcess locates the definition of a named process. The editor's own buffers win over
// disk: the file on disk may be older than what the reader is looking at.
func (s *Server) findProcess(name string) (string, defdoc.Range, bool) {
	for uri, text := range s.docs {
		if r, ok := processNamed(text, name); ok {
			return uri, r, true
		}
	}
	seen := 0
	for _, root := range s.folders {
		var found string
		var span defdoc.Range
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || found != "" || seen > maxScanned {
				return nil
			}
			if d.IsDir() {
				if skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") && d.Name() != "." {
					return filepath.SkipDir
				}
				return nil
			}
			if !isDefinitionURI(path) {
				return nil
			}
			seen++
			uri := pathToURI(path)
			if _, open := s.docs[uri]; open {
				return nil // already searched, and the buffer is the newer text
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			if r, ok := processNamed(string(data), name); ok {
				found, span = uri, r
			}
			return nil
		})
		if found != "" {
			return found, span, true
		}
	}
	return "", defdoc.Range{}, false
}

// processNamed returns where `name` is written, in the document of text that declares it.
func processNamed(text, name string) (defdoc.Range, bool) {
	docs, err := defdoc.ParseAll([]byte(text))
	if err != nil {
		return defdoc.Range{}, false
	}
	for _, doc := range docs {
		v, ok := doc.ValueAt("name")
		if !ok || v != name {
			continue
		}
		span, ok := doc.Span("name")
		if !ok {
			continue
		}
		return span.Value, true
	}
	return defdoc.Range{}, false
}

// uriToPath and pathToURI convert only what this server sees: `file:` URIs for local files.
// A URI with any other scheme has no path, and is skipped rather than guessed at.
func uriToPath(uri string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return "", false
	}
	return u.Path, true
}

func pathToURI(path string) string {
	u := url.URL{Scheme: "file", Path: path}
	return u.String()
}
