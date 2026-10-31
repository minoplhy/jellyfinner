package edgerouter

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jellyfinner/internal/config"
	"jellyfinner/internal/queue"
)

const (
	itemID = "0123456789abcdef0123456789abcdef"
	sign   = "signing-key-signing-key-signing-key"
)

var hash = strings.Repeat("a", 64)

type fakeJellyfin struct {
	users map[string]string
	items map[string]string
	calls int
}

func (f *fakeJellyfin) Username(_ context.Context, token string) (string, error) {
	f.calls++
	if u, ok := f.users[token]; ok {
		return u, nil
	}
	return "", errors.New("401")
}

func (f *fakeJellyfin) ItemPath(_ context.Context, id string) (string, error) {
	if p, ok := f.items[id]; ok {
		return p, nil
	}
	return "", errors.New("no item")
}

type fixture struct {
	t      *testing.T
	router *Router
	store  *queue.Store
	jf     *fakeJellyfin
	hits   *int
	now    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := queue.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := &fixture{t: t, store: store, now: time.Unix(2000000, 0), hits: new(int)}
	store.Now = func() time.Time { return f.now }
	f.jf = &fakeJellyfin{users: map[string]string{"alice-token": "alice", "bob-token": "bob"}, items: map[string]string{itemID: "/lib/Movies/A.mp4"}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*f.hits++
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(upstream.Close)
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		LibraryDir: "/lib", EdgeURLTTL: 24 * time.Hour,
		EdgeNodes:   []config.EdgeNode{{Name: "edge1", URL: "https://edge1.example.com", SigningKey: sign}},
		EdgeViewers: map[string]string{"alice": "edge1"},
	}
	f.router = NewRouter(cfg, store, f.jf, u, log.New(io.Discard, "", 0))
	return f
}

func (f *fixture) cached() {
	f.t.Helper()
	if err := f.store.AddPlacement(&queue.Placement{Node: "edge1", SHA256: hash, Output: "Movies/A.mp4", Size: 5}); err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.ActivatePlacement("edge1", hash); err != nil {
		f.t.Fatal(err)
	}
	f.healthy()
}

func (f *fixture) healthy() {
	f.t.Helper()
	if err := f.store.NodeSeen("edge1", 1, 1); err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.SetNodeHealth("edge1", true); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) get(path string, headers map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, r)
	return w
}

func stream(token string) string {
	return "/Videos/" + itemID + "/stream.mp4?Static=true&api_key=" + token
}

func TestRedirectsAssignedViewer(t *testing.T) {
	f := newFixture(t)
	f.cached()
	w := f.get(stream("alice-token"), nil)
	if w.Code != http.StatusFound {
		t.Fatalf("code = %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://edge1.example.com/e/"+hash+".mp4?md5=") {
		t.Errorf("location = %s", loc)
	}
	if strings.Contains(loc, "alice-token") {
		t.Error("the Jellyfin token must never reach the edge")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("a signed URL must not be cached")
	}
	if ps, _ := f.store.NodePlacements("edge1"); ps[0].LastPlayed == 0 {
		t.Error("the play was not recorded")
	}
}

func TestStreamURLForms(t *testing.T) {
	f := newFixture(t)
	f.cached()
	for _, p := range []string{
		"/videos/" + itemID + "/stream?static=true&api_key=alice-token",
		"/Videos/" + itemID + "/stream.mkv?Static=True&ApiKey=alice-token",
		"/Videos/" + itemID + "/msid/stream.mp4?static=true&api_key=alice-token",
	} {
		if w := f.get(p, nil); w.Code != http.StatusFound {
			t.Errorf("%s = %d", p, w.Code)
		}
	}
	for name, headers := range map[string]map[string]string{
		"x-emby":       {"X-Emby-Token": "alice-token"},
		"mediabrowser": {"Authorization": `MediaBrowser Client="x", Token="alice-token"`},
		"emby-auth":    {"X-Emby-Authorization": `MediaBrowser Token="alice-token"`},
	} {
		if w := f.get("/Videos/"+itemID+"/stream?static=true", headers); w.Code != http.StatusFound {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
}

func TestEverythingElsePassesThrough(t *testing.T) {
	f := newFixture(t)
	f.cached()
	tests := map[string]string{
		"not assigned": stream("bob-token"),
		"bad token":    stream("stolen"),
		"no token":     "/Videos/" + itemID + "/stream.mp4?static=true",
		"not static":   "/Videos/" + itemID + "/stream.mp4?api_key=alice-token",
		"static false": "/Videos/" + itemID + "/stream.mp4?static=false&api_key=alice-token",
		"hls":          "/Videos/" + itemID + "/master.m3u8?static=true&api_key=alice-token",
		"other route":  "/Users/Me",
		"unknown item": "/Videos/ffffffffffffffffffffffffffffffff/stream?static=true&api_key=alice-token",
		"not an id":    "/Videos/..%2f..%2fetc/stream?static=true&api_key=alice-token",
		"subtitle":     "/Videos/" + itemID + "/msid/Subtitles/0/stream.srt?api_key=alice-token",
	}
	for name, p := range tests {
		if w := f.get(p, nil); w.Code != http.StatusTeapot {
			t.Errorf("%s: code = %d, want a pass to Jellyfin", name, w.Code)
		}
	}
}

func TestPostIsNeverRedirected(t *testing.T) {
	f := newFixture(t)
	f.cached()
	r := httptest.NewRequest(http.MethodPost, stream("alice-token"), nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Errorf("code = %d", w.Code)
	}
}

func TestFailsOpenToFR(t *testing.T) {
	cases := map[string]func(*fixture){
		"not cached": func(*fixture) {},
		"pulling": func(f *fixture) {
			f.store.AddPlacement(&queue.Placement{Node: "edge1", SHA256: hash, Output: "Movies/A.mp4", Size: 5})
			f.healthy()
		},
		"deleting": func(f *fixture) {
			f.cached()
			f.store.SetPlacementState("edge1", hash, queue.PlacementDeleting, "")
		},
		"failed": func(f *fixture) {
			f.cached()
			f.store.SetPlacementState("edge1", hash, queue.PlacementFailed, "x")
		},
		"probe failing": func(f *fixture) {
			f.cached()
			f.store.SetNodeHealth("edge1", false)
		},
		"agent silent": func(f *fixture) {
			f.cached()
			f.now = f.now.Add(2 * HealthWindow)
		},
		"database closed": func(f *fixture) {
			f.cached()
			f.store.Close()
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			setup(f)
			if w := f.get(stream("alice-token"), nil); w.Code != http.StatusTeapot {
				t.Errorf("code = %d, want a pass to Jellyfin", w.Code)
			}
		})
	}
}

func TestContainerLibraryPath(t *testing.T) {
	f := newFixture(t)
	f.router.cfg.JellyfinLibraryDir = "/media"
	f.jf.items[itemID] = "/media/Movies/A.mp4"
	f.cached()
	if w := f.get(stream("alice-token"), nil); w.Code != http.StatusFound {
		t.Errorf("code = %d", w.Code)
	}
}

func TestUserLookupIsCached(t *testing.T) {
	f := newFixture(t)
	f.cached()
	f.get(stream("alice-token"), nil)
	f.get(stream("alice-token"), nil)
	if f.jf.calls != 1 {
		t.Errorf("seeks must not each call Jellyfin: %d", f.jf.calls)
	}
	f.now = f.now.Add(2 * userCacheFor)
	f.healthy()
	f.get(stream("alice-token"), nil)
	if f.jf.calls != 2 {
		t.Errorf("a revoked token must stop working soon: %d", f.jf.calls)
	}
}
