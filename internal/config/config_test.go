package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const token = "0123456789abcdef0123456789abcdef"

func base() map[string]string {
	return map[string]string{
		"ENCODE_MODE":      "auto",
		"DB_PATH":          "/var/lib/j/db",
		"ORIGINALS_DIR":    "/srv/originals",
		"LIBRARY_DIR":      "/srv/library",
		"INCOMING_DIR":     "/srv/incoming",
		"REMOTE_ORIGINALS": "origin:/srv/originals/",
		"REMOTE_INCOMING":  "origin:/srv/incoming",
	}
}

func parseMap(m map[string]string) (*Config, error) {
	return parse(func(k string) string { return m[k] })
}

func TestParseValid(t *testing.T) {
	c, err := parseMap(base())
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != ModeAuto || c.RemoteOriginals != "origin:/srv/originals" || c.MaxAttempts != 3 {
		t.Errorf("config = %+v", c)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name, key, value string
		want             string
	}{
		{"missing mode", "ENCODE_MODE", "", "required"},
		{"invalid mode", "ENCODE_MODE", "both", "must be auto or manual"},
		{"missing library", "LIBRARY_DIR", "", "LIBRARY_DIR"},
		{"library in originals", "LIBRARY_DIR", "/srv/originals/library", "contain each other"},
		{"originals in library", "ORIGINALS_DIR", "/srv/library/x", "contain each other"},
		{"bad duration", "STABLE_FOR", "soon", "STABLE_FOR"},
		{"cert without key", "TLS_CERT", "/c.pem", "together"},
		{"zero attempts", "MAX_ATTEMPTS", "0", "at least 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := base()
			m[tt.key] = tt.value
			_, err := parseMap(m)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestManualRequiresJellyfinAndWebhook(t *testing.T) {
	m := base()
	m["ENCODE_MODE"] = "manual"
	if _, err := parseMap(m); err == nil || !strings.Contains(err.Error(), "JELLYFIN_URL") {
		t.Errorf("err = %v", err)
	}
	m["JELLYFIN_URL"], m["JELLYFIN_TOKEN"] = "http://127.0.0.1:8096", "t"
	if _, err := parseMap(m); err == nil || !strings.Contains(err.Error(), "WEBHOOK_TOKEN") {
		t.Errorf("err = %v", err)
	}
	m["WEBHOOK_TOKEN"] = token
	if _, err := parseMap(m); err != nil {
		t.Error(err)
	}
}

func TestEdgeViewers(t *testing.T) {
	m := base()
	m["EDGE_VIEWERS"] = "alice=edge1;bob=edge2"
	c, err := parseMap(m)
	if err != nil {
		t.Fatal(err)
	}
	if c.EdgeViewers["alice"] != "edge1" || c.EdgeViewers["bob"] != "edge2" {
		t.Errorf("viewers = %+v", c.EdgeViewers)
	}
	m["EDGE_VIEWERS"] = "alice"
	if _, err := parseMap(m); err == nil {
		t.Error("a viewer without a node must be refused")
	}
}

func TestCheckFilesystems(t *testing.T) {
	root := t.TempDir()
	c := &Config{IncomingDir: filepath.Join(root, "i"), LibraryDir: filepath.Join(root, "l")}
	if err := c.CheckFilesystems(); err == nil {
		t.Error("missing directories must fail")
	}
	for _, d := range []string{c.IncomingDir, c.LibraryDir} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.CheckFilesystems(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{c.IncomingDir, c.LibraryDir} {
		entries, err := os.ReadDir(d)
		if err != nil || len(entries) != 0 {
			t.Errorf("%s not left clean: %v, %v", d, entries, err)
		}
	}
}

func TestCheckControlURL(t *testing.T) {
	for url, ok := range map[string]bool{
		"https://control.example.com": true,
		"http://10.8.0.1:8200":        true,
		"http://127.0.0.1:8200":       true,
		"http://[fd00::1]:8200":       true,
		"http://8.8.8.8:8200":         false,
		"http://control.example.com":  false,
		"http://orchestrator:8200":    true,
		"http://host.docker.internal": true,
		"http://:8200":                false,
		"http://internal.example.com": false,
		"ftp://10.8.0.1":              false,
		"10.8.0.1:8200":               false,
	} {
		if err := CheckControlURL(url); (err == nil) != ok {
			t.Errorf("%s: err = %v, want ok=%v", url, err, ok)
		}
	}
}

func TestCheckListenAddr(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:8200": true,
		"10.8.0.1:8200":  true,
		"8.8.8.8:8200":   false,
		"0.0.0.0:8200":   false,
		"localhost:8200": false,
		"8200":           false,
	} {
		if err := CheckListenAddr(addr); (err == nil) != ok {
			t.Errorf("%s: err = %v, want ok=%v", addr, err, ok)
		}
	}
	t.Setenv("JELLYFINNER_IN_CONTAINER", "1")
	if err := CheckListenAddr("0.0.0.0:8200"); err != nil {
		t.Errorf("in a container: %v", err)
	}
	if err := CheckListenAddr("8.8.8.8:8200"); err == nil {
		t.Error("a public address stays refused in a container")
	}
}

func TestLoadEnvFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), ".env")
	body := "# comment\n\nexport ENCODE_MODE=\"auto\"\n"
	for k, v := range base() {
		if k != "ENCODE_MODE" {
			body += k + "='" + v + "'\n"
		}
	}
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != ModeAuto || c.LibraryDir != "/srv/library" {
		t.Errorf("config = %+v", c)
	}

	t.Setenv("LIBRARY_DIR", "/srv/other")
	if c, err = Load(file); err != nil || c.LibraryDir != "/srv/other" {
		t.Errorf("the environment must override the file: %+v, %v", c, err)
	}
}
