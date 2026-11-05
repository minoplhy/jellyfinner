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
	"sync"
	"testing"
	"time"

	"jellyfinner/internal/jellyfin/jellyfintest"
)

const itemID = "0123456789abcdef0123456789abcdef"

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

// fakeResolver answers as the orchestrator would, from a table keyed by
// lowercased username and library path.
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string]string
	err     error
	delay   time.Duration
	calls   int
	asked   []string // "user output", as received
}

func (r *fakeResolver) Resolve(ctx context.Context, user, output string) (string, error) {
	r.mu.Lock()
	r.calls++
	r.asked = append(r.asked, user+" "+output)
	answer, err, delay := r.answers[strings.ToLower(user)+" "+output], r.err, r.delay
	r.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return answer, err
}

type fixture struct {
	t        *testing.T
	router   *Router
	resolver *fakeResolver
	jf       *fakeJellyfin
	hits     *int
	now      time.Time

	seen *http.Request // the last request upstream received
}

const edgeURL = "https://edge1.example.com/e/" + "aaaa.mp4?md5=x&expires=1"

func newFixture(t *testing.T) *fixture { return newFixtureAt(t, "") }

// newFixtureAt puts Jellyfin under a Base URL, such as /jf.
func newFixtureAt(t *testing.T, base string) *fixture {
	t.Helper()
	f := &fixture{t: t, now: time.Unix(2000000, 0), hits: new(int), resolver: &fakeResolver{answers: map[string]string{}}}
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
	f.router = NewRouter(Settings{LibraryDir: "/lib"}, f.resolver, f.jf, u, log.New(io.Discard, "", 0))
	f.router.Now = func() time.Time { return f.now }
	return f
}

// cached makes the orchestrator send alice to her edge for Movies/A.mp4.
func (f *fixture) cached() { f.resolver.answers["alice Movies/A.mp4"] = edgeURL }

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
	if w.Code != http.StatusFound || w.Header().Get("Location") != edgeURL {
		t.Fatalf("code = %d, location = %s", w.Code, w.Header().Get("Location"))
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("a signed URL must not be cached")
	}
	// The orchestrator gets the username and the file inside the library,
	// never the viewer's Jellyfin token.
	if len(f.resolver.asked) != 1 || f.resolver.asked[0] != "alice Movies/A.mp4" {
		t.Errorf("asked = %q", f.resolver.asked)
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
		"origin answer": stream("bob-token"),
		"bad token":     stream("stolen"),
		"no token":      "/Videos/" + itemID + "/stream.mp4?static=true",
		"not static":    "/Videos/" + itemID + "/stream.mp4?api_key=alice-token",
		"static false":  "/Videos/" + itemID + "/stream.mp4?static=false&api_key=alice-token",
		"hls":           "/Videos/" + itemID + "/master.m3u8?static=true&api_key=alice-token",
		"other route":   "/Users/Me",
		"unknown item":  "/Videos/ffffffffffffffffffffffffffffffff/stream?static=true&api_key=alice-token",
		"not an id":     "/Videos/..%2f..%2fetc/stream?static=true&api_key=alice-token",
		"subtitle":      "/Videos/" + itemID + "/msid/Subtitles/0/stream.srt?api_key=alice-token",
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

// An orchestrator that fails, or is slower than the timeout, means the origin,
// and the failure is not remembered.
func TestFailsOpenToOrigin(t *testing.T) {
	f := newFixture(t)
	f.cached()
	f.resolver.err = errors.New("orchestrator down")
	if w := f.get(stream("alice-token"), nil); w.Code != http.StatusTeapot {
		t.Errorf("error: code = %d", w.Code)
	}
	f.resolver.err = nil
	f.resolver.delay = time.Second
	f.router.ResolveTimeout = 20 * time.Millisecond
	start := time.Now()
	if w := f.get(stream("alice-token"), nil); w.Code != http.StatusTeapot {
		t.Errorf("slow: code = %d", w.Code)
	}
	if waited := time.Since(start); waited > 500*time.Millisecond {
		t.Errorf("waited %s for a slow orchestrator", waited)
	}
	f.resolver.delay = 0
	if w := f.get(stream("alice-token"), nil); w.Code != http.StatusFound {
		t.Errorf("recovered: code = %d", w.Code)
	}
}

// One answer covers repeated requests, as on a seek, for a short while.
func TestAnswerIsCachedBriefly(t *testing.T) {
	f := newFixture(t)
	f.cached()
	f.get(stream("alice-token"), nil)
	f.get(stream("alice-token"), nil)
	f.get(stream("bob-token"), nil)
	if f.resolver.calls != 2 {
		t.Errorf("calls = %d, want one per viewer", f.resolver.calls)
	}
	// "Origin" is cached too, then asked again once it ages.
	f.resolver.answers["bob Movies/A.mp4"] = edgeURL
	if w := f.get(stream("bob-token"), nil); w.Code != http.StatusTeapot {
		t.Errorf("bob within the cache = %d", w.Code)
	}
	f.now = f.now.Add(answerCacheFor + time.Second)
	if w := f.get(stream("bob-token"), nil); w.Code != http.StatusFound {
		t.Errorf("bob after the cache = %d", w.Code)
	}
}

func TestContainerLibraryPath(t *testing.T) {
	f := newFixture(t)
	f.router.settings.LibraryDir = "/media"
	f.jf.items[itemID] = "/media/Movies/A.mp4"
	f.cached()
	if w := f.get(stream("alice-token"), nil); w.Code != http.StatusFound {
		t.Errorf("code = %d", w.Code)
	}
	// A file outside the library is never asked about.
	f.jf.items[itemID] = "/elsewhere/A.mp4"
	f.now = f.now.Add(itemCacheFor + time.Second)
	calls := f.resolver.calls
	if w := f.get(stream("alice-token"), nil); w.Code != http.StatusTeapot || f.resolver.calls != calls {
		t.Errorf("outside the library: code = %d, calls %d", w.Code, f.resolver.calls-calls)
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
