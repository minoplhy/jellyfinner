package web

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"jellyfinner/internal/config"
	"jellyfinner/internal/database"
	"jellyfinner/internal/database/dbtest"
	"jellyfinner/internal/jellyfin"
	"jellyfinner/internal/jellyfin/jellyfintest"
	"jellyfinner/internal/model"
	"jellyfinner/internal/notify"
	"jellyfinner/internal/orchestrator"
)

type fakeAuth struct {
	jellyfintest.Unimplemented
	ended []string
	calls int
}

func (f *fakeAuth) Authenticate(_ context.Context, username, password string) (bool, string, error) {
	f.calls++
	switch {
	case username == "admin" && password == "pw":
		return true, "tok-admin", nil
	case username == "viewer" && password == "pw":
		return false, "tok-viewer", nil
	}
	return false, "", jellyfin.ErrBadLogin
}

func (f *fakeAuth) EndSession(_ context.Context, token string) error {
	f.ended = append(f.ended, token)
	return nil
}

type fixture struct {
	t      *testing.T
	svc    *orchestrator.Service
	store  database.Store
	cfg    *config.Config
	auth   *fakeAuth
	server *httptest.Server
	now    time.Time
}

func newFixture(t *testing.T) *fixture { return newFixtureAt(t, "") }

// newFixtureAt serves the UI under a base path, such as /admin.
func newFixtureAt(t *testing.T, base string) *fixture {
	t.Helper()
	root := t.TempDir()
	adminURL := ""
	if base != "" {
		adminURL = "http://admin.test" + base
	}
	cfg := &config.Config{
		AdminURL: adminURL,
		Mode:     config.ModeAuto, OriginalsDir: filepath.Join(root, "o"), LibraryDir: filepath.Join(root, "l"),
		IncomingDir: filepath.Join(root, "i"), RemoteOriginals: "origin:/o", RemoteIncoming: "origin:/i",
		LeaseFor: time.Minute, MaxAttempts: 2, StableFor: time.Minute, DeleteGrace: time.Hour, MassDeletePercent: 20,
	}
	for _, d := range []string{cfg.OriginalsDir, cfg.LibraryDir, cfg.IncomingDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{t: t, cfg: cfg, auth: &fakeAuth{}, now: time.Unix(2000000, 0)}
	clock := func() time.Time { return f.now }
	store := dbtest.Open(t, database.Options{Now: clock})
	f.store = store
	logger := log.New(io.Discard, "", 0)
	f.svc = orchestrator.New(cfg, store, notify.New("", ""), nil, logger)
	f.svc.Now = clock
	if err := f.svc.Nodes.Add(&model.Node{Name: "worker1", Kind: model.NodeWorker, TokenHash: "x"}); err != nil {
		t.Fatal(err)
	}
	seen := func() map[string]time.Time { return map[string]time.Time{"worker1": f.now} }
	f.server = httptest.NewServer(New(f.svc, f.auth, seen, logger).Handler())
	t.Cleanup(f.server.Close)
	return f
}

func (f *fixture) add(path string) {
	f.t.Helper()
	full := filepath.Join(f.cfg.OriginalsDir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("x"), 0o640); err != nil {
		f.t.Fatal(err)
	}
	if err := f.svc.ScanOnce(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

// client is a browser with a cookie jar that does not follow redirects.
func (f *fixture) client() *http.Client {
	f.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (f *fixture) login(c *http.Client, user, password string) int {
	f.t.Helper()
	resp, err := c.PostForm(f.server.URL+"/login", url.Values{"username": {user}, "password": {password}})
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func (f *fixture) admin() *http.Client {
	f.t.Helper()
	c := f.client()
	if got := f.login(c, "admin", "pw"); got != http.StatusSeeOther {
		f.t.Fatalf("login = %d", got)
	}
	return c
}

func (f *fixture) get(c *http.Client, path string) (int, string) {
	f.t.Helper()
	resp, err := c.Get(f.server.URL + path)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

var csrfField = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

func (f *fixture) csrf(c *http.Client) string {
	f.t.Helper()
	_, body := f.get(c, "/library")
	m := csrfField.FindStringSubmatch(body)
	if m == nil {
		f.t.Fatal("no CSRF token on the page")
	}
	return m[1]
}

func (f *fixture) post(c *http.Client, path string, form url.Values) (int, string) {
	f.t.Helper()
	resp, err := c.PostForm(f.server.URL+path, form)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestEveryPageNeedsLogin(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	for _, path := range []string{"/", "/queue", "/library", "/nodes", "/usage", "/jobs/x", "/jobs/x/log"} {
		got, _ := f.get(c, path)
		if got != http.StatusSeeOther {
			t.Errorf("GET %s = %d", path, got)
		}
	}
	for _, path := range []string{"/library/add", "/library/reencode", "/library/reencode/confirm", "/jobs/x/cancel", "/logout",
		"/scan", "/backfill/start", "/nodes/add", "/nodes/worker1/rotate", "/nodes/worker1/delete", "/nodes/worker1/pause", "/cloud/pause", "/cloud/servers/x/drain"} {
		got, _ := f.post(c, path, url.Values{})
		if got != http.StatusUnauthorized {
			t.Errorf("POST %s = %d", path, got)
		}
	}
	for _, path := range []string{"/fragments/queue", "/fragments/dashboard", "/fragments/library", "/fragments/nodes/workers", "/fragments/usage", "/fragments/jobs/x"} {
		req, err := http.NewRequest(http.MethodGet, f.server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("HX-Request", "true")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("an expired %s must not swap in the login page: %d", path, resp.StatusCode)
		}
	}
}

func TestLogin(t *testing.T) {
	f := newFixture(t)
	if got := f.login(f.client(), "viewer", "pw"); got != http.StatusUnauthorized {
		t.Errorf("a non-admin must be refused: %d", got)
	}
	if got := f.login(f.client(), "admin", "wrong"); got != http.StatusUnauthorized {
		t.Errorf("wrong password: %d", got)
	}
	c := f.admin()
	if got, body := f.get(c, "/"); got != http.StatusOK || !strings.Contains(body, "Dashboard") {
		t.Errorf("dashboard = %d", got)
	}
	if len(f.auth.ended) != 2 || f.auth.ended[0] != "tok-viewer" || f.auth.ended[1] != "tok-admin" {
		t.Errorf("Jellyfin sessions must be closed at once: %v", f.auth.ended)
	}
}

func TestLoginResponsesMatchForBadUserAndNonAdmin(t *testing.T) {
	f := newFixture(t)
	first, _ := f.post(f.client(), "/login", url.Values{"username": {"viewer"}, "password": {"pw"}})
	_, a := f.post(f.client(), "/login", url.Values{"username": {"viewer"}, "password": {"pw"}})
	_, b := f.post(f.client(), "/login", url.Values{"username": {"nobody"}, "password": {"x"}})
	if first != http.StatusUnauthorized || a != b {
		t.Error("the page must not confirm which accounts exist")
	}
}

func TestLoginRateLimit(t *testing.T) {
	f := newFixture(t)
	c := f.client()
	for range loginLimit {
		f.login(c, "admin", "wrong")
	}
	calls := f.auth.calls
	if got := f.login(c, "admin", "pw"); got != http.StatusTooManyRequests {
		t.Errorf("after too many failures: %d", got)
	}
	if f.auth.calls != calls {
		t.Error("a blocked attempt must not reach Jellyfin")
	}
	f.now = f.now.Add(2 * loginWindow)
	if got := f.login(c, "admin", "pw"); got != http.StatusSeeOther {
		t.Errorf("after the window: %d", got)
	}
}

func TestSessionExpiresAndLogoutEnds(t *testing.T) {
	f := newFixture(t)
	c := f.admin()
	f.now = f.now.Add(sessionLife + time.Minute)
	if got, _ := f.get(c, "/"); got != http.StatusSeeOther {
		t.Errorf("expired session = %d", got)
	}

	c = f.admin()
	token := f.csrf(c)
	if got, _ := f.post(c, "/logout", url.Values{"csrf": {token}}); got != http.StatusSeeOther {
		t.Fatalf("logout = %d", got)
	}
	if got, _ := f.get(c, "/"); got != http.StatusSeeOther {
		t.Errorf("after logout = %d", got)
	}
}

func TestCookieFlags(t *testing.T) {
	f := newFixture(t)
	resp, err := f.client().PostForm(f.server.URL+"/login", url.Values{"username": {"admin"}, "password": {"pw"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie = %+v", cookie)
	}
}

// An https ADMIN_URL marks the cookie secure even when a proxy in front sends
// no X-Forwarded-Proto.
func TestHTTPSAdminURLMakesCookieSecure(t *testing.T) {
	f := newFixture(t)
	f.cfg.AdminURL = "https://admin.test"
	resp, err := f.client().PostForm(f.server.URL+"/login", url.Values{"username": {"admin"}, "password": {"pw"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == cookieName && !c.Secure {
			t.Errorf("cookie = %+v", c)
		}
	}
}

func TestCSRFRequired(t *testing.T) {
	f := newFixture(t)
	f.add("a.mkv")
	c := f.admin()
	for _, form := range []url.Values{{"path": {"a.mkv"}}, {"path": {"a.mkv"}, "csrf": {"wrong"}}} {
		if got, _ := f.post(c, "/library/add", form); got != http.StatusForbidden {
			t.Errorf("form %v = %d", form, got)
		}
	}
	jobs, err := f.store.List(10)
	if err != nil || len(jobs) != 0 {
		t.Errorf("a refused form queued work: %+v, %v", jobs, err)
	}
}

func TestCSRFIsPerSession(t *testing.T) {
	f := newFixture(t)
	f.add("a.mkv")
	one, two := f.admin(), f.admin()
	if got, _ := f.post(two, "/library/add", url.Values{"path": {"a.mkv"}, "csrf": {f.csrf(one)}}); got != http.StatusForbidden {
		t.Errorf("another session's token = %d", got)
	}
}

func TestLibraryAddAndQueueActions(t *testing.T) {
	f := newFixture(t)
	f.add("a.mkv")
	c := f.admin()
	token := f.csrf(c)
	if got, _ := f.post(c, "/library/add", url.Values{"csrf": {token}, "path": {"a.mkv"}}); got != http.StatusSeeOther {
		t.Fatalf("add = %d", got)
	}
	jobs, err := f.store.List(10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
	id := jobs[0].ID
	if got, body := f.get(c, "/queue"); got != http.StatusOK || !strings.Contains(body, "a.mkv") {
		t.Errorf("queue = %d", got)
	}
	if got, _ := f.post(c, "/jobs/"+id+"/cancel", url.Values{"csrf": {token}}); got != http.StatusSeeOther {
		t.Errorf("cancel = %d", got)
	}
	if j, _ := f.store.Get(id); j.State != model.StateCancelled {
		t.Errorf("state = %s", j.State)
	}
	if got, _ := f.post(c, "/jobs/"+id+"/retry", url.Values{"csrf": {token}}); got != http.StatusSeeOther {
		t.Errorf("retry = %d", got)
	}
	if got, _ := f.post(c, "/jobs/"+id+"/delete", url.Values{"csrf": {token}}); got != http.StatusNotFound {
		t.Errorf("unknown action = %d", got)
	}
	if _, body := f.get(c, "/fragments/queue"); !strings.Contains(body, "Cancel") {
		t.Error("fragment missing actions")
	}
	if got, _ := f.get(c, "/jobs/nope"); got != http.StatusNotFound {
		t.Errorf("unknown job = %d", got)
	}
}

func TestReencodeShowsSizeThenNeedsReason(t *testing.T) {
	f := newFixture(t)
	f.add("a.mkv")
	c := f.admin()
	token := f.csrf(c)

	got, body := f.post(c, "/library/reencode", url.Values{"csrf": {token}, "path": {"a.mkv"}})
	if got != http.StatusOK || !strings.Contains(body, "This pulls") {
		t.Fatalf("confirm = %d", got)
	}
	jobs, _ := f.store.List(10)
	if len(jobs) != 0 {
		t.Fatal("the confirmation step must not order anything")
	}
	if got, _ := f.post(c, "/library/reencode/confirm", url.Values{"csrf": {token}, "path": {"a.mkv"}}); got != http.StatusSeeOther {
		t.Fatalf("no reason = %d", got)
	}
	if jobs, _ = f.store.List(10); len(jobs) != 0 {
		t.Fatal("a reason is required")
	}
	form := url.Values{"csrf": {token}, "path": {"a.mkv"}, "reason": {"audio"}, "force_encode": {"on"}}
	if got, _ := f.post(c, "/library/reencode/confirm", form); got != http.StatusSeeOther {
		t.Fatalf("order = %d", got)
	}
	jobs, _ = f.store.List(10)
	if len(jobs) != 1 || jobs[0].Reason != "audio" || !jobs[0].ForceEncode || jobs[0].Software {
		t.Errorf("jobs = %+v", jobs)
	}
}

func TestHostileNamesAreEscaped(t *testing.T) {
	f := newFixture(t)
	f.add(`<script>alert(1)<script>.mkv`)
	c := f.admin()
	_, body := f.get(c, "/library")
	if strings.Contains(body, "<script>alert") {
		t.Error("a file name reached the page unescaped")
	}
	_, body = f.get(c, "/library?q=<img src=x>")
	if strings.Contains(body, "<img src=x>") {
		t.Error("the search box reflected unescaped input")
	}
}

func TestSecurityHeaders(t *testing.T) {
	f := newFixture(t)
	resp, err := f.client().Get(f.server.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	policy := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(policy, "script-src 'self'") || strings.Contains(policy, "unsafe") {
		t.Errorf("policy = %q", policy)
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Error("clickjacking header missing")
	}
}

func TestStaticAssets(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/static/htmx.min.js", "/static/app.css", "/static/app.js"} {
		if got, _ := f.get(f.client(), path); got != http.StatusOK {
			t.Errorf("%s = %d", path, got)
		}
	}
}

func TestDashboardShowsNodeAndBackfill(t *testing.T) {
	f := newFixture(t)
	f.add("a.mkv")
	c := f.admin()
	_, body := f.get(c, "/fragments/dashboard")
	for _, want := range []string{"worker1", "online", "Stopped", "1 titles left"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
}

func TestNodesPageAndCacheAction(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.Nodes.Add(&model.Node{Name: "edge1", Kind: model.NodeEdge, TokenHash: "x", URL: "https://edge1.example.com", QuotaBytes: 1000}); err != nil {
		t.Fatal(err)
	}
	f.add("a.mkv")
	info, err := os.Stat(filepath.Join(f.cfg.OriginalsDir, "a.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetEncoded("a.mkv", info.Size(), info.ModTime().UnixNano(), "", strings.Repeat("a", 64), 5); err != nil {
		t.Fatal(err)
	}
	c := f.admin()
	token := f.csrf(c)
	if _, body := f.get(c, "/library"); !strings.Contains(body, "Cache to node") {
		t.Error("library has no cache action")
	}
	form := url.Values{"csrf": {token}, "path": {"a.mkv"}, "node": {"edge1"}}
	if got, _ := f.post(c, "/library/cache", form); got != http.StatusSeeOther {
		t.Fatalf("cache = %d", got)
	}
	if got, _ := f.post(c, "/library/cache", url.Values{"path": {"a.mkv"}, "node": {"edge1"}}); got != http.StatusForbidden {
		t.Errorf("cache without CSRF = %d", got)
	}
	got, body := f.get(c, "/nodes")
	if got != http.StatusOK || !strings.Contains(body, "a.mp4") || !strings.Contains(body, "pulling") {
		t.Errorf("nodes = %d", got)
	}
	remove := url.Values{"csrf": {token}, "output": {"a.mp4"}}
	if got, _ := f.post(c, "/nodes/edge1/remove", remove); got != http.StatusSeeOther {
		t.Errorf("remove = %d", got)
	}
	if ps, _ := f.store.NodePlacements("edge1"); ps[0].State != model.PlacementDeleting {
		t.Errorf("state = %s", ps[0].State)
	}
}

// The dashboard and the Nodes page judge a node the same way, and the Nodes
// page lists encoding nodes as well as edge nodes.
func TestNodeStatesAgreeAcrossPages(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"edge1", "edge2", "edge3"} {
		if err := f.svc.Nodes.Add(&model.Node{Name: name, Kind: model.NodeEdge, TokenHash: "x" + name, URL: "https://" + name + ".example.com", QuotaBytes: 1000}); err != nil {
			t.Fatal(err)
		}
	}
	// worker2 has made no call since the orchestrator started.
	if err := f.svc.Nodes.Add(&model.Node{Name: "worker2", Kind: model.NodeWorker, TokenHash: "xw2"}); err != nil {
		t.Fatal(err)
	}
	// edge1 reports and passes its probe, edge2 reports but fails it, and
	// edge3 has never reported.
	for _, name := range []string{"edge1", "edge2"} {
		if err := f.store.NodeSeen(name, 500, 1000); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.SetNodeHealth("edge1", true); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetNodeHealth("edge2", false); err != nil {
		t.Fatal(err)
	}
	c := f.admin()
	_, dash := f.get(c, "/fragments/dashboard")
	code, nodes := f.get(c, "/nodes")
	if code != http.StatusOK {
		t.Fatalf("nodes = %d", code)
	}
	badge := regexp.MustCompile(`(?s)node-(\w+)".*?class="badge b-(\w+)"`)
	states := func(body string) map[string]string {
		got := map[string]string{}
		for _, m := range badge.FindAllStringSubmatch(body, -1) {
			got[m[1]] = m[2]
		}
		return got
	}
	want := map[string]string{"worker1": "online", "worker2": "unseen", "edge1": "online", "edge2": "unhealthy", "edge3": "unseen"}
	for page, body := range map[string]string{"dashboard": dash, "nodes": nodes} {
		got := states(body)
		for name, state := range want {
			if got[name] != state {
				t.Errorf("%s: %s = %q, want %q", page, name, got[name], state)
			}
		}
	}
	for _, text := range []string{"Encoding nodes", "Edge nodes", "unhealthy: probe failing", "never reported", "not seen since restart"} {
		if !strings.Contains(nodes, text) {
			t.Errorf("nodes page missing %q", text)
		}
	}

}

func TestNoNodesConfiguredHidesCacheAction(t *testing.T) {
	f := newFixture(t)
	f.add("a.mkv")
	c := f.admin()
	if _, body := f.get(c, "/library"); strings.Contains(body, "Cache to node") {
		t.Error("no node, no cache action")
	}
	if got, body := f.get(c, "/nodes"); got != http.StatusOK || !strings.Contains(body, "No edge nodes") {
		t.Errorf("nodes = %d", got)
	}
}

func TestBasePath(t *testing.T) {
	f := newFixtureAt(t, "/admin")
	c := f.client()
	do := func(method, path string, form url.Values) *http.Response {
		t.Helper()
		var resp *http.Response
		var err error
		if method == http.MethodPost {
			resp, err = c.PostForm(f.server.URL+path, form)
		} else {
			resp, err = c.Get(f.server.URL + path)
		}
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	if resp := do(http.MethodGet, "/admin/", nil); resp.Header.Get("Location") != "/admin/login" {
		t.Errorf("redirect = %q", resp.Header.Get("Location"))
	}
	// A proxy that strips the prefix sends the bare path.
	if code, _ := f.get(c, "/login"); code != http.StatusOK {
		t.Errorf("stripped login page = %d", code)
	}
	code, body := f.get(c, "/admin/login")
	if code != http.StatusOK || !strings.Contains(body, `href="/admin/static/app.css"`) || !strings.Contains(body, `action="/admin/login"`) {
		t.Errorf("login page = %d: %s", code, body)
	}
	if resp := do(http.MethodGet, "/admin/static/app.css", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("stylesheet = %d", resp.StatusCode)
	}

	resp := do(http.MethodPost, "/admin/login", url.Values{"username": {"admin"}, "password": {"pw"}})
	if resp.Header.Get("Location") != "/admin/" {
		t.Errorf("after login = %q", resp.Header.Get("Location"))
	}
	if cookies := resp.Cookies(); len(cookies) != 1 || cookies[0].Path != "/admin" {
		t.Errorf("cookies = %+v", cookies)
	}

	f.add("a.mkv")
	_, page := f.get(c, "/admin/library")
	m := csrfField.FindStringSubmatch(page)
	if m == nil || !strings.Contains(page, `action="/admin/library/add"`) {
		t.Fatalf("library page: %s", page)
	}
	resp = do(http.MethodPost, "/admin/library/add", url.Values{"csrf": {m[1]}, "path": {"a.mkv"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/library" {
		t.Errorf("form post = %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestPauseAndResumeWorker(t *testing.T) {
	f := newFixture(t)
	c := f.admin()
	token := f.csrf(c)
	if code, _ := f.post(c, "/nodes/worker1/pause", url.Values{"csrf": {token}}); code != http.StatusSeeOther {
		t.Fatalf("pause = %d", code)
	}
	nodes, err := f.store.LoadNodes()
	if err != nil || !nodes[0].Paused {
		t.Fatalf("worker1 not paused: %+v, %v", nodes, err)
	}
	_, body := f.get(c, "/nodes")
	if !strings.Contains(body, "paused, online") || !strings.Contains(body, "/nodes/worker1/resume") {
		t.Error("the Nodes page must show the pause and offer resume")
	}
	if code, _ := f.post(c, "/nodes/worker1/resume", url.Values{"csrf": {token}}); code != http.StatusSeeOther {
		t.Fatalf("resume = %d", code)
	}
	if nodes, _ := f.store.LoadNodes(); nodes[0].Paused {
		t.Error("worker1 still paused")
	}
	if code, _ := f.post(c, "/nodes/worker1/explode", url.Values{"csrf": {token}}); code != http.StatusNotFound {
		t.Errorf("unknown action = %d", code)
	}
}
