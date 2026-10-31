package doctor

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReport(t *testing.T) {
	var out bytes.Buffer
	r := New(&out)
	r.Check("good", nil)
	r.Note("worth seeing")
	if err := r.Done(); err != nil {
		t.Errorf("clean run: %v", err)
	}
	r.Check("bad", errors.New("broken"))
	if err := r.Done(); err == nil {
		t.Error("a failure must fail the run")
	}
	for _, want := range []string{"PASS  good", "NOTE  worth seeing", "FAIL  bad: broken"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}

func TestFolders(t *testing.T) {
	dir := t.TempDir()
	if err := Writable(dir); err != nil {
		t.Error(err)
	}
	if err := Readable(dir); err != nil {
		t.Errorf("an empty folder is fine: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
	missing := filepath.Join(dir, "missing")
	if Writable(missing) == nil || Readable(missing) == nil {
		t.Error("a missing folder must fail")
	}
}

func TestFreeSpace(t *testing.T) {
	dir := t.TempDir()
	if err := FreeSpace(dir, 0); err != nil {
		t.Error(err)
	}
	if err := FreeSpace(dir, 1<<40); err == nil {
		t.Error("nobody has that much free")
	}
}
