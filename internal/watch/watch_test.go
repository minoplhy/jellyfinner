package watch

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"jellyfinner/internal/model"
)

func write(t *testing.T, root, rel string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestScanFiltersFiles(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{
		"Movies/A/A.mkv", "Movies/B/B.MP4", "TV/S/e1.ts",
		"Movies/A/A.nfo", "Movies/A/poster.jpg",
		"Movies/C/C.mkv.part", "Movies/C/C.mkv.!qB", "Movies/D/.D.mkv",
		".hidden/x.mkv", "Movies/@eaDir/x.mkv",
	} {
		write(t, root, f)
	}
	files, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for path := range files {
		got = append(got, path)
	}
	slices.Sort(got)
	want := []string{"Movies/A/A.mkv", "Movies/B/B.MP4", "TV/S/e1.ts"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestScanMissingRootFails(t *testing.T) {
	if _, err := Scan(filepath.Join(t.TempDir(), "gone")); err == nil {
		t.Error("a missing root must fail, not look like an empty library")
	}
}

func TestScanUnreadableDirFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	root := t.TempDir()
	write(t, root, "Movies/A/A.mkv")
	locked := filepath.Join(root, "Movies")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o750) })
	if _, err := Scan(root); err == nil {
		t.Error("a partial listing must fail the scan, or files look deleted")
	}
}

func src(path string, size, mtime int64) *model.Source {
	return &model.Source{Path: path, Size: size, MtimeNS: mtime}
}

func TestClassify(t *testing.T) {
	gone := src("gone.mkv", 9, 9)
	away := src("away.mkv", 8, 8)
	away.MissingSince = 100
	moved := src("old/name.mkv", 5, 5)
	tracked := []*model.Source{src("same.mkv", 1, 1), src("edit.mkv", 2, 2), gone, away, moved}

	c := Classify(tracked, map[string]File{
		"same.mkv":     {1, 1},
		"edit.mkv":     {2, 3},
		"away.mkv":     {8, 8},
		"new.mkv":      {7, 7},
		"new/name.mkv": {5, 5},
	})

	if !slices.Equal(c.New, []string{"new.mkv"}) {
		t.Errorf("New = %v", c.New)
	}
	if !slices.Equal(c.Changed, []string{"edit.mkv"}) {
		t.Errorf("Changed = %v", c.Changed)
	}
	if !slices.Equal(c.Returned, []string{"away.mkv"}) {
		t.Errorf("Returned = %v", c.Returned)
	}
	if len(c.Renamed) != 1 || c.Renamed[0].From != moved || c.Renamed[0].To != "new/name.mkv" {
		t.Errorf("Renamed = %+v", c.Renamed)
	}
	if len(c.Missing) != 1 || c.Missing[0] != gone {
		t.Errorf("Missing = %+v", c.Missing)
	}
}

func TestClassifyAmbiguousRenameIsNotGuessed(t *testing.T) {
	one, two := src("a.mkv", 5, 5), src("b.mkv", 5, 5)

	c := Classify([]*model.Source{one, two}, map[string]File{"c.mkv": {5, 5}})
	if len(c.Renamed) != 0 || len(c.Missing) != 2 || len(c.New) != 1 {
		t.Errorf("two sources, one target: %+v", c)
	}

	c = Classify([]*model.Source{one}, map[string]File{"c.mkv": {5, 5}, "d.mkv": {5, 5}})
	if len(c.Renamed) != 0 || len(c.Missing) != 1 || len(c.New) != 2 {
		t.Errorf("one source, two targets: %+v", c)
	}
}
