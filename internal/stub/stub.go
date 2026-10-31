// Package stub manages the placeholder files of manual mode. Every stub is a
// hardlink to one master file, so a stub is recognised by the filesystem alone
// and a whole library of them costs one small file.
package stub

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed stub.mp4
var clip []byte

// Master is the file every stub links to. It must share a filesystem with the
// library, and it must stay outside any Jellyfin library.
type Master string

// Ensure writes the master if it is missing or out of date. Existing stubs keep
// linking to the old copy, so a changed clip only reaches new stubs. The mode
// is read-only, because a write would change every stub at once.
func (m Master) Ensure() error {
	current, err := os.ReadFile(string(m))
	if err == nil && bytes.Equal(current, clip) {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := string(m) + ".tmp"
	if err := os.WriteFile(tmp, clip, 0o444); err != nil {
		return err
	}
	if err := os.Rename(tmp, string(m)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Create links a stub at target. An existing file is left alone, so a real
// encode can never be replaced by a stub.
func (m Master) Create(target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	err := os.Link(string(m), target)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create stub: %w", err)
	}
	return nil
}

// Is reports whether target is a stub. A missing file is not one.
func (m Master) Is(target string) (bool, error) {
	master, err := os.Stat(string(m))
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(master, info), nil
}
