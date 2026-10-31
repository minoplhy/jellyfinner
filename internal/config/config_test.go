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
		{"short token", "NODE_TOKENS", "worker1=short", "32 characters"},
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

func TestEdgeConfig(t *testing.T) {
	key := strings.Repeat("k", 32)
	m := base()
	m["NODE_TOKENS"] = "edge1=" + token
	m["EDGE_NODES"] = "edge1=https://edge1.example.com,500," + key
	m["EDGE_TOKEN_KEY"] = key
	m["EDGE_VIEWERS"] = "alice=edge1"
	c, err := parseMap(m)
	if err != nil {
		t.Fatal(err)
	}
	if n := c.EdgeNode("edge1"); n == nil || n.QuotaBytes != 500e9 || c.EdgeViewers["alice"] != "edge1" {
		t.Errorf("config = %+v", c)
	}
	for name, change := range map[string][2]string{
		"plain http":     {"EDGE_NODES", "edge1=http://edge1.example.com,500," + key},
		"zero quota":     {"EDGE_NODES", "edge1=https://edge1.example.com,0," + key},
		"short key":      {"EDGE_NODES", "edge1=https://edge1.example.com,500,short"},
		"no token":       {"NODE_TOKENS", "other=" + token},
		"no token key":   {"EDGE_TOKEN_KEY", ""},
		"unknown viewer": {"EDGE_VIEWERS", "alice=jp"},
	} {
		bad := map[string]string{}
		for k, v := range m {
			bad[k] = v
		}
		bad[change[0]] = change[1]
		if _, err := parseMap(bad); err == nil {
			t.Errorf("%s: want an error", name)
		}
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

func TestParseNodes(t *testing.T) {
	nodes, err := ParseNodes("worker1=" + token + "@1.2.3.4/32+10.0.0.0/8; edge1=" + token)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].Name != "worker1" || len(nodes[0].Allow) != 2 || len(nodes[1].Allow) != 0 {
		t.Errorf("nodes = %+v", nodes)
	}
	for _, bad := range []string{"worker1", "=" + token, "worker1=" + token + "@nonsense", "worker1=" + token + ";worker1=" + token} {
		if _, err := ParseNodes(bad); err == nil {
			t.Errorf("%q: want an error", bad)
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
		"ftp://10.8.0.1":              false,
		"10.8.0.1:8200":               false,
	} {
		if err := CheckControlURL(url); (err == nil) != ok {
			t.Errorf("%s: err = %v, want ok=%v", url, err, ok)
		}
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
