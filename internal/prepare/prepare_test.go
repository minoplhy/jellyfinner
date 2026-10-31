package prepare

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func mine() string { return strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()) }

func TestParse(t *testing.T) {
	if uid, gid, err := ParseOwner("10001:101"); err != nil || uid != 10001 || gid != 101 {
		t.Errorf("owner = %d %d %v", uid, gid, err)
	}
	for _, bad := range []string{"", "10001", "a:b", "-1:0", "1:"} {
		if _, _, err := ParseOwner(bad); err == nil {
			t.Errorf("owner %q: want an error", bad)
		}
	}
	if d, err := ParseDir("/edge:2750"); err != nil || d.Path != "/edge" || d.Mode != 0o2750 {
		t.Errorf("dir = %+v %v", d, err)
	}
	if d, err := ParseDir("/edge"); err != nil || d.Mode != 0 {
		t.Errorf("dir = %+v %v", d, err)
	}
	for _, bad := range []string{"", ":750", "/edge:rwx", "/edge:999"} {
		if _, err := ParseDir(bad); err == nil {
			t.Errorf("dir %q: want an error", bad)
		}
	}
}

func TestCreatesMissingFoldersWithMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	if err := Command([]string{"--owner", mine(), dir + ":2750"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o750 || info.Mode()&os.ModeSetgid == 0 {
		t.Errorf("mode = %v", info.Mode())
	}
}

func TestLeavesContentsAlone(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "episode.mkv")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Command([]string{"--owner", mine(), dir + ":755"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("a file inside changed: %v", info.Mode())
	}
}

func TestRefusalNamesTheFolder(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can give a folder to anyone")
	}
	dir := t.TempDir()
	err := Command([]string{"--owner", "10001:10001", dir})
	if err == nil || !strings.Contains(err.Error(), dir) {
		t.Errorf("err = %v, want it to name %s", err, dir)
	}
}

func TestCommandNeedsAFolder(t *testing.T) {
	for _, args := range [][]string{nil, {"--owner", "1:1"}, {"/edge", "1:1", "x"}} {
		if err := Command(args); err == nil {
			t.Errorf("%v: want an error", args)
		}
	}
}

func TestWaitReturnsOnceTheFolderExists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "late")
	go func() {
		time.Sleep(50 * time.Millisecond)
		os.Mkdir(dir, 0o755)
	}()
	Wait(10*time.Second, dir)
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("returned before the folder appeared: %v", err)
	}
}

func TestWaitGivesUp(t *testing.T) {
	start := time.Now()
	Wait(20*time.Millisecond, filepath.Join(t.TempDir(), "never"))
	if time.Since(start) > 5*time.Second {
		t.Error("the wait must end at its timeout")
	}
}
