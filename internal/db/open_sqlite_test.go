package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenSQLite_NamesAnUnwritableDirectory(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes anywhere")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) }) // runs before TempDir's removal, which needs it
	_, err := OpenSQLite(filepath.Join(dir, "d.db"), "FULL", WithFullFsync())
	if err == nil || !strings.Contains(err.Error(), dir+" is not writable") {
		t.Fatalf("err = %v; SQLite alone says \"no such file or directory\", which sends the reader looking for a missing file", err)
	}
}

func TestOpenSQLite_LeavesAMissingDirectoryAlone(t *testing.T) {
	_, err := OpenSQLite(filepath.Join(t.TempDir(), "nope", "d.db"), "FULL")
	if err == nil || strings.Contains(err.Error(), "not writable") {
		t.Fatalf("err = %v; a directory that does not exist is not a permission problem", err)
	}
}
