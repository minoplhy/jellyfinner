package edgeagent

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jellyfinner/internal/edge"
)

func TestFileServer(t *testing.T) {
	dir := t.TempDir()
	hash := strings.Repeat("a", 64)
	if err := os.WriteFile(filepath.Join(dir, hash+".mp4"), []byte("0123456789"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, tmpDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tmpDir, hash+".part"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000_000, 0)
	srv := httptest.NewServer(&FileServer{Dir: dir, Key: "key", Now: func() time.Time { return now }})
	defer srv.Close()

	signed := edge.SignURL(srv.URL, "key", hash, now.Add(time.Hour))
	get := func(target, rng string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	resp := get(signed, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "video/mp4" {
		t.Errorf("valid URL: %s, %s", resp.Status, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" || resp.Header.Get("Cache-Control") != "private, no-store" {
		t.Errorf("headers = %v", resp.Header)
	}
	if resp := get(signed, "bytes=2-5"); resp.StatusCode != http.StatusPartialContent || resp.ContentLength != 4 {
		t.Errorf("range: %s, length %d", resp.Status, resp.ContentLength)
	}

	u, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	altered := *u
	q.Set("expires", "9999999999")
	altered.RawQuery = q.Encode()
	other := strings.Replace(signed, hash, strings.Repeat("b", 64), 1)
	tests := map[string]struct {
		target string
		want   int
	}{
		"altered expiry": {altered.String(), http.StatusForbidden},
		"other file":     {other, http.StatusForbidden},
		"wrong key":      {edge.SignURL(srv.URL, "other", hash, now.Add(time.Hour)), http.StatusForbidden},
		"no signature":   {srv.URL + edge.FilePath(hash), http.StatusForbidden},
		"expired":        {edge.SignURL(srv.URL, "key", hash, now.Add(-time.Second)), http.StatusGone},
		"unknown path":   {srv.URL + "/etc/passwd", http.StatusNotFound},
		"partial files":  {srv.URL + "/.tmp/" + hash + ".part", http.StatusNotFound},
		"traversal":      {srv.URL + "/e/..%2f.tmp%2f" + hash + ".mp4", http.StatusNotFound},
	}
	for name, tt := range tests {
		if resp := get(tt.target, ""); resp.StatusCode != tt.want {
			t.Errorf("%s: %s, want %d", name, resp.Status, tt.want)
		}
	}
	if resp := get(srv.URL+"/health", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("health: %s", resp.Status)
	}
	if resp := get(edge.SignURL(srv.URL, "key", strings.Repeat("c", 64), now.Add(time.Hour)), ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing file: %s", resp.Status)
	}
}
