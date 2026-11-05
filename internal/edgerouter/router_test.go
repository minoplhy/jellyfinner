package edgerouter

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"jellyfinner/internal/config"
	"jellyfinner/internal/database"
	"jellyfinner/internal/database/dbtest"
	"jellyfinner/internal/jellyfin/jellyfintest"
	"jellyfinner/internal/model"
)

const (
	itemID = "0123456789abcdef0123456789abcdef"
	sign   = "signing-key-signing-key-signing-key"
)

var hash = strings.Repeat("a", 64)

type fakeJellyfin struct {
	jellyfintest.Unimplemented
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
	store  database.Store
	jf     *fakeJellyfin
	hits   *int
	now    time.Time

	seen *http.Request // the last request upstream received
}

func newFixture(t *testing.T) *fixture { return newFixtureAt(t, "") }

// newFixtureAt puts Jellyfin under a Base URL, such as /jf.
func newFixtureAt(t *testing.T, base string) *fixture {
	t.Helper()
	f := &fixture{t: t, now: time.Unix(2000000, 0), hits: new(int)}
	clock := func() time.Time { return f.now }
	store := dbtest.Open(t, database.Options{Now: clock})
	f.store = store
	f.jf = &fakeJellyfin{users: map[string]string{"alice-token": "alice", "bob-token": "bob"}, items: map[string]string{itemID: "/lib/Movies/A.mp4"}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*f.hits++
		f.seen = r
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(upstream.Close)
	u, err := url.Parse(upstream.URL + base)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{LibraryDir: "/lib", EdgeURLTTL: 24 * time.Hour}
	if err := store.AddNode(&model.Node{ID: "edge1", Name: "edge1", Kind: model.NodeEdge, TokenHash: "x", URL: "https://edge1.example.com", SigningKey: sign}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetViewer("alice", "edge1"); err != nil {
		t.Fatal(err)
	}
	f.router = NewRouter(cfg, store, f.jf, u, log.New(io.Discard, "", 0))
	f.router.Now = clock
	return f
}

func (f *fixture) cached() {
	f.t.Helper()
	if err := f.store.AddPlacement(&model.Placement{Node: "edge1", SHA256: hash, Output: "Movies/A.mp4", Size: 5}); err != nil {
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

// Viewers refer to the edge by ID, so a rename and a new URL change nothing
// but where the redirect points. Usernames match without regard to case.
func TestViewerFollowsAnEditedEdge(t *testing.T) {
	f := newFixture(t)
	if err := f.store.SetViewer("BOB", "edge1"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateNode(&model.Node{ID: "edge1", Name: "edge-paris", URL: "https://example.com/edge1"}); err != nil {
		t.Fatal(err)
	}
	f.cached()
	for _, token := range []string{"alice-token", "bob-token"} {
		w := f.get(stream(token), nil)
		if loc := w.Header().Get("Location"); w.Code != http.StatusFound || !strings.HasPrefix(loc, "https://example.com/edge1/e/"+hash+".mp4?md5=") {
			t.Errorf("%s: %d %s", token, w.Code, loc)
		}
	}
	if err := f.store.RemoveViewer("bob"); err != nil {
		t.Fatal(err)
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

func TestFailsOpenToOrigin(t *testing.T) {
	cases := map[string]func(*fixture){
		"not cached": func(*fixture) {},
		"pulling": func(f *fixture) {
			f.store.AddPlacement(&model.Placement{Node: "edge1", SHA256: hash, Output: "Movies/A.mp4", Size: 5})
			f.healthy()
		},
		"deleting": func(f *fixture) {
			f.cached()
			f.store.SetPlacementState("edge1", hash, model.PlacementDeleting, "")
		},
		"failed": func(f *fixture) {
			f.cached()
			f.store.SetPlacementState("edge1", hash, model.PlacementFailed, "x")
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

func TestJellyfinBaseURL(t *testing.T) {
	f := newFixtureAt(t, "/jf")
	f.cached()
	if w := f.get("/jf"+stream("alice-token"), nil); w.Code != http.StatusFound {
		t.Errorf("a stream under the Base URL: code = %d, want a redirect", w.Code)
	}
	if w := f.get("/jf/Items?x=1", nil); w.Code != http.StatusTeapot || f.seen.URL.Path != "/jf/Items" || f.seen.URL.RawQuery != "x=1" {
		t.Errorf("passthrough reached %q, code %d", f.seen.URL.String(), w.Code)
	}
	// A proxy that strips the prefix sends the bare path.
	if w := f.get(stream("alice-token"), nil); w.Code != http.StatusFound {
		t.Errorf("a stripped stream: code = %d", w.Code)
	}
	if w := f.get("/jfx"+stream("alice-token"), nil); w.Code == http.StatusFound {
		t.Error("a neighbouring path is not under the Base URL")
	}
}

func TestPassthroughKeepsForwardedHeaders(t *testing.T) {
	f := newFixture(t)
	f.get("/Items", map[string]string{"X-Forwarded-For": "198.51.100.7", "X-Forwarded-Proto": "https", "Host": "tv.example.com"})
	if got := f.seen.Header.Get("X-Forwarded-For"); got != "198.51.100.7, 192.0.2.1" {
		t.Errorf("X-Forwarded-For = %q", got)
	}
	if got := f.seen.Header.Get("X-Forwarded-Proto"); got != "https" {
		t.Errorf("an outer proxy's https must survive: %q", got)
	}
}
