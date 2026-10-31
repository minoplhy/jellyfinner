// Package doctor reports whether a node can work, from inside its container, so
// it sees the same mounts, users and network the service does. It only reads,
// apart from a short-lived test file.
package doctor

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// Report collects the results of one run.
type Report struct {
	out   io.Writer
	fails int
}

// New writes results to out as they happen, so a hang shows where it stuck.
func New(out io.Writer) *Report { return &Report{out: out} }

// Check records one result.
func (r *Report) Check(name string, err error) {
	if err != nil {
		r.fails++
		fmt.Fprintf(r.out, "FAIL  %s: %v\n", name, err)
		return
	}
	fmt.Fprintf(r.out, "PASS  %s\n", name)
}

// Note records something worth seeing that is not a failure.
func (r *Report) Note(text string) { fmt.Fprintf(r.out, "NOTE  %s\n", text) }

// Done prints the total, and fails if any check did.
func (r *Report) Done() error {
	if r.fails > 0 {
		return fmt.Errorf("%d check(s) failed", r.fails)
	}
	fmt.Fprintln(r.out, "All checks passed")
	return nil
}

// Writable proves a folder can be written by making a file there.
func Writable(dir string) error {
	f, err := os.CreateTemp(dir, ".jellyfinner-doctor-")
	if err != nil {
		return fmt.Errorf("%w. This process runs as %d:%d, so check the folder's owner", err, os.Getuid(), os.Getgid())
	}
	f.Close()
	return os.Remove(f.Name())
}

// Readable proves a folder can be listed.
func Readable(dir string) error {
	f, err := os.Open(filepath.Clean(dir))
	if err == nil {
		defer f.Close()
		_, err = f.Readdirnames(1)
		if err == io.EOF {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("%w. This process runs as %d:%d, so check the folder's permissions", err, os.Getuid(), os.Getgid())
	}
	return nil
}

// FreeSpace fails with less than wantGB free.
func FreeSpace(dir string, wantGB uint64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return err
	}
	free := st.Bavail * uint64(st.Bsize) / 1e9
	if free < wantGB {
		return fmt.Errorf("%s has %d GB free, want at least %d", dir, free, wantGB)
	}
	return nil
}
