package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"genroc/internal/api"
	"genroc/internal/errcode"
)

// errorPages generates both halves and returns the definition-language page and the two
// concatenated. They land in different sections now, so a test reading one directory would
// silently assert against half the vocabulary.
func errorPages(t *testing.T) (task string, both string) {
	t.Helper()
	defDir, httpDir := t.TempDir(), t.TempDir()
	if err := writeErrorReference(defDir, httpDir); err != nil {
		t.Fatalf("writeErrorReference: %v", err)
	}
	read := func(dir string) string {
		b, err := os.ReadFile(filepath.Join(dir, "errors.md"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	task = read(defDir)
	return task, task + read(httpDir)
}

// Every code the engine can store must reach the page. The two vocabularies are assembled from
// separate accessors, so a code added to one and missed by the generator produces a page that
// still looks complete — and a code nobody documents is one an author meets for the first time
// in a failed instance.
func TestEveryCodeReachesAPage(t *testing.T) {
	_, page := errorPages(t)

	codes := errcode.All()
	if len(codes) < 15 {
		t.Fatalf("errcode reports %d codes, which cannot be right", len(codes))
	}
	for _, info := range codes {
		if !strings.Contains(page, "| `"+string(info.Code)+"` |") {
			t.Errorf("task code %q reaches no page", info.Code)
		}
	}
	for _, c := range api.ReferenceCodes() {
		if !strings.Contains(page, "| `"+string(c.Code)+"` |") {
			t.Errorf("API code %q reaches no page", c.Code)
		}
	}
}

// The two vocabularies share no codes and must not share a page: `not_found` in an on_error rule
// is refused at registration, and a reader who met both in one table has been invited to try it.
func TestTheTwoVocabulariesStaySeparate(t *testing.T) {
	task, _ := errorPages(t)
	for _, c := range api.ReferenceCodes() {
		if strings.Contains(task, "| `"+string(c.Code)+"` |") {
			t.Errorf("API code %q is listed among the codes an on_error rule can name", c.Code)
		}
	}
}

// A terminal code has no reporting task, and the Reported-by column belongs to the catchable
// table alone. An empty cell there would read as a code nobody classified.
func TestTerminalCodesCarryNoReportingKind(t *testing.T) {
	for _, info := range errcode.Terminal() {
		if names := info.Kinds.Names(); len(names) != 0 {
			t.Errorf("%q reports kinds %v, but nothing catches a terminal code", info.Code, names)
		}
	}
	if got := codeList(nil); got != "—" {
		t.Errorf("an empty kind set rendered %q, which reads as a cell nobody filled", got)
	}
}
