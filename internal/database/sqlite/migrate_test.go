package sqlite

import (
	"path/filepath"
	"testing"

	"jellyfinner/internal/database"
)

func TestMigrateRunsOnceAndInOrder(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"), database.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// The store already has the real migrations, so the test steps follow them.
	steps := append(append([]string{}, migrations...), `CREATE TABLE m1 (x INTEGER)`, `ALTER TABLE m1 ADD COLUMN y INTEGER`)
	first := len(migrations) + 1
	if err := migrate(s.db, steps[:first]); err != nil {
		t.Fatal(err)
	}
	// Running a step twice would fail, so a pass proves only the new one ran.
	if err := migrate(s.db, steps); err != nil {
		t.Fatal(err)
	}
	if err := migrate(s.db, steps); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO m1 (x, y) VALUES (1, 2)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(s.db, steps[:first]); err == nil {
		t.Error("an older program must refuse a newer database")
	}
}
