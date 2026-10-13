package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"genroc/internal/db"
)

func TestSeedSuppliedTokens_RefusesAnUnknownPermissionBeforeStoringAny(t *testing.T) {
	f, err := os.CreateTemp("", "genroc-seed-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	database, err := db.OpenSQLite(f.Name(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	good, _ := db.NewTokenSecret()
	typo, _ := db.NewTokenSecret()
	_, err = seedSuppliedTokens(database, "ci=deploy="+good+"\nadmin=amdin="+typo+"\n")
	if err == nil {
		t.Fatal("a seed granting `amdin` was accepted; it mints a token that authorizes nothing")
	}
	for _, want := range []string{`"amdin"`, "admin, deploy, operate, read, worker"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q; it must name the bad permission and the valid set (missing %s)", err, want)
		}
	}
	if strings.Contains(err.Error(), typo) {
		t.Errorf("err = %q carries the secret, and it goes to the startup log", err)
	}
	rows, err := database.ListTokens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("%d tokens stored by a refused seed list; the whole list is checked before any write", len(rows))
	}
}

// The file is what an operator edits by hand, so its perms read as `genctl token create`'s do.
func TestParseSeedTokens_OneEntryPerLineWithCommaSeparatedPerms(t *testing.T) {
	ci, _ := db.NewTokenSecret()
	worker, _ := db.NewTokenSecret()
	entries, noSecret, err := parseSeedTokens("# operators\nci=deploy,read=" + ci + "\r\n\n  evaluator=worker=" + worker + "\nadmin=admin=\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].label != "ci" || entries[1].label != "evaluator" {
		t.Fatalf("entries = %+v; want ci and evaluator, with the comment and blank line skipped", entries)
	}
	if got := strings.Join(entries[0].perms, ","); got != "deploy,read" {
		t.Errorf("ci perms = %q, want deploy,read", got)
	}
	if strings.Join(noSecret, ",") != "admin" {
		t.Errorf("noSecret = %v; a blanked secret must be reported by label", noSecret)
	}
	if _, _, err := parseSeedTokens("ci=deploy+read=" + ci); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("err = %v; `+` is not a separator, and the error must say which line", err)
	}
}
