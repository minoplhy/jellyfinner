package edgeagent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jellyfinner/internal/edge"
)

// Only a probe signed with the node's key learns the build.
func TestHealthTellsBuildOnlyWhenSigned(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	srv := httptest.NewServer(&FileServer{Dir: t.TempDir(), Key: "key", Now: func() time.Time { return now }})
	defer srv.Close()
	build := func(url string) string {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %s", url, resp.Status)
		}
		return resp.Header.Get(edge.BuildHeader)
	}
	if got := build(srv.URL + "/health"); got != "" {
		t.Errorf("unsigned probe got build %q", got)
	}
	if got := build(edge.SignHealthURL(srv.URL, "wrong", now.Add(time.Minute))); got != "" {
		t.Errorf("wrong key got build %q", got)
	}
	if got := build(edge.SignHealthURL(srv.URL, "key", now.Add(-time.Minute))); got != "" {
		t.Errorf("expired probe got build %q", got)
	}
	if got := build(edge.SignHealthURL(srv.URL+"/edge1", "key", now.Add(time.Minute))); !strings.HasPrefix(got, "jellyfinner-edge-serve/") {
		t.Errorf("signed probe under a path got build %q", got)
	}
}
