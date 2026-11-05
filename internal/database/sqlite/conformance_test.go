package sqlite_test

import (
	"path/filepath"
	"testing"

	"jellyfinner/internal/database"
	"jellyfinner/internal/database/dbtest"
	"jellyfinner/internal/database/sqlite"
)

func TestConformance(t *testing.T) {
	dbtest.Run(t, func(t *testing.T, opts database.Options) database.Store {
		s, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), opts)
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestRegistered(t *testing.T) {
	s, err := database.Open(sqlite.Driver, filepath.Join(t.TempDir(), "state.db"), database.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := database.Open("nope", "", database.Options{}); err == nil {
		t.Error("an unknown driver must be refused")
	}
}
