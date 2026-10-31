package stub

import (
	"os"
	"path/filepath"
	"testing"
)

func master(t *testing.T) (Master, string) {
	t.Helper()
	dir := t.TempDir()
	m := Master(filepath.Join(dir, ".stub.mp4"))
	if err := m.Ensure(); err != nil {
		t.Fatal(err)
	}
	return m, dir
}

func TestCreateAndIdentify(t *testing.T) {
	m, dir := master(t)
	target := filepath.Join(dir, "Movies/A/A.mp4")
	if err := m.Create(target); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.Is(target); err != nil || !ok {
		t.Errorf("Is = %v, %v", ok, err)
	}
	if ok, err := m.Is(filepath.Join(dir, "missing.mp4")); err != nil || ok {
		t.Errorf("a missing file is not a stub: %v, %v", ok, err)
	}
}

func TestCreateNeverReplacesAnEncode(t *testing.T) {
	m, dir := master(t)
	target := filepath.Join(dir, "A.mp4")
	if err := os.WriteFile(target, []byte("real encode"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Create(target); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "real encode" {
		t.Errorf("content = %q, %v", data, err)
	}
	if ok, _ := m.Is(target); ok {
		t.Error("an encode must not read as a stub")
	}
}

func TestReplacingAStubLeavesOthers(t *testing.T) {
	m, dir := master(t)
	one, two := filepath.Join(dir, "one.mp4"), filepath.Join(dir, "two.mp4")
	for _, f := range []string{one, two} {
		if err := m.Create(f); err != nil {
			t.Fatal(err)
		}
	}
	encode := filepath.Join(dir, "encode")
	if err := os.WriteFile(encode, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(encode, one); err != nil {
		t.Fatal(err)
	}
	if ok, _ := m.Is(one); ok {
		t.Error("a published title must stop being a stub")
	}
	if ok, _ := m.Is(two); !ok {
		t.Error("other stubs must be untouched")
	}
}

func TestEnsureIsReadOnlyAndRepeatable(t *testing.T) {
	m, _ := master(t)
	if err := m.Ensure(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(string(m))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Errorf("mode = %v: a write would change every stub", info.Mode())
	}
}

func TestClipIsDirectPlayMP4(t *testing.T) {
	if len(clip) < 1024 || string(clip[4:8]) != "ftyp" {
		t.Errorf("embedded clip is not an MP4 (%d bytes)", len(clip))
	}
}
