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
	_, err = seedSuppliedTokens(database, "ci=deploy="+good+",admin=amdin="+typo)
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
