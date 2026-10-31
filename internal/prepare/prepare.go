// Package prepare readies data folders from inside a container, so a compose
// file needs no host script. It runs in a one-shot service, once as root,
// because the long-running services never are. It must stay free of cgo, and of
// anything a shell would provide, because the edge image has neither.
package prepare

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"jellyfinner/internal/doctor"
)

// WaitFor is how long a service waits for its one-shot prepare service.
const WaitFor = 30 * time.Second

// Dir is a folder to prepare. A Mode of 0 leaves the mode alone.
type Dir struct {
	Path string
	Mode uint32
}

// ParseOwner reads "uid:gid".
func ParseOwner(s string) (uid, gid int, err error) {
	u, g, ok := strings.Cut(s, ":")
	if ok {
		if uid, err = strconv.Atoi(u); err == nil {
			gid, err = strconv.Atoi(g)
		}
	}
	if !ok || err != nil || uid < 0 || gid < 0 {
		return 0, 0, fmt.Errorf("owner %q must be uid:gid", s)
	}
	return uid, gid, nil
}

// ParseDir reads "path" or "path:octalmode".
func ParseDir(s string) (Dir, error) {
	path, mode, ok := strings.Cut(s, ":")
	if path == "" {
		return Dir{}, fmt.Errorf("folder %q has no path", s)
	}
	if !ok {
		return Dir{Path: path}, nil
	}
	m, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return Dir{}, fmt.Errorf("folder %q: mode must be octal", s)
	}
	return Dir{Path: path, Mode: uint32(m)}, nil
}

// Run creates each folder and sets its owner. It never recurses, so it cannot
// disturb existing media, and it changes nothing that is already right, so a
// network share set up by hand works without chown rights.
func Run(uid, gid int, dirs []Dir) error {
	var errs []error
	for _, d := range dirs {
		if err := one(uid, gid, d); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func one(uid, gid int, d Dir) error {
	if err := os.MkdirAll(d.Path, 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", d.Path, err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(d.Path, &st); err != nil {
		return fmt.Errorf("cannot read %s: %w", d.Path, err)
	}
	if int(st.Uid) != uid || int(st.Gid) != gid {
		if err := os.Chown(d.Path, uid, gid); err != nil {
			return fmt.Errorf("cannot give %s to %d:%d: %w. On a network share, set the owner on the server instead", d.Path, uid, gid, err)
		}
	}
	if d.Mode != 0 && st.Mode&0o7777 != d.Mode {
		// syscall, because os.Chmod spells the setgid bit differently.
		if err := syscall.Chmod(d.Path, d.Mode); err != nil {
			return fmt.Errorf("cannot set the mode of %s: %w", d.Path, err)
		}
	}
	return nil
}

// Wait gives the prepare service time to finish, by polling until each folder is
// writable. Compose files cannot simply depend on that service, because Podman
// then refuses to restart a crashed service while the one-shot sits exited. After
// the timeout it returns anyway, so the caller fails with its own clear error.
func Wait(timeout time.Duration, dirs ...string) {
	deadline := time.Now().Add(timeout)
	for _, dir := range dirs {
		for doctor.Writable(dir) != nil && time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
		}
	}
}

// Command is the prepare subcommand: prepare --owner UID:GID PATH[:MODE]...
func Command(args []string) error {
	if len(args) < 3 || args[0] != "--owner" {
		return errors.New("usage: prepare --owner UID:GID PATH[:MODE]...")
	}
	uid, gid, err := ParseOwner(args[1])
	if err != nil {
		return err
	}
	dirs := make([]Dir, 0, len(args)-2)
	for _, arg := range args[2:] {
		d, err := ParseDir(arg)
		if err != nil {
			return err
		}
		dirs = append(dirs, d)
	}
	return Run(uid, gid, dirs)
}
