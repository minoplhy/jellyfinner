package web

import (
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"jellyfinner/internal/config"
	"jellyfinner/internal/model"
)

// hx sends a request the way htmx does, from a page at current.
func (f *fixture) hx(c *http.Client, method, path string, form url.Values, current string) (*http.Response, string) {
	f.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, f.server.URL+path, body)
	if err != nil {
		f.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("HX-Request", "true")
	if current != "" {
		req.Header.Set("HX-Current-URL", f.server.URL+current)
	}
	resp, err := c.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp, string(b)
}

func (f *fixture) queued(path string) string {
	f.t.Helper()
	f.add(path)
	id, err := f.svc.Add(path)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

// An htmx action gets the redrawn part and a toast, never a redirect, and the
// redrawn queue keeps the filter of the page it came from.
func TestHTMXJobActionRedrawsQueueInPlace(t *testing.T) {
	f := newFixture(t)
	id := f.queued("a.mkv")
	c := f.admin()
	token := f.csrf(c)
	resp, body := f.hx(c, http.MethodPost, "/jobs/"+id+"/cancel", url.Values{"csrf": {token}, "from": {"queue"}}, "/queue?state=cancelled&q=a")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("htmx cancel = %d", resp.StatusCode)
	}
	for _, want := range []string{`id="queue-body"`, `hx-swap-oob="beforeend"`, "Job cancelled.", `/fragments/queue?q=a&amp;state=cancelled"`, "a.mkv"} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing %q:\n%s", want, body)
		}
	}
	// A failed action still answers 200, with the error as a toast.
	_, body = f.hx(c, http.MethodPost, "/jobs/"+id+"/bump", url.Values{"csrf": {token}}, "/queue")
	if !strings.Contains(body, "toast-error") {
		t.Errorf("bumping a cancelled job must report an error: %s", body)
	}
	// From the job page, the job view is redrawn instead.
	_, body = f.hx(c, http.MethodPost, "/jobs/"+id+"/retry", url.Values{"csrf": {token}, "from": {"job"}}, "/jobs/"+id)
	if !strings.Contains(body, `id="job"`) || strings.Contains(body, `id="queue-body"`) || !strings.Contains(body, "Queued again.") {
		t.Errorf("job page action: %s", body)
	}
	// Without htmx, the same action from the job page lands back on it.
	if got, _ := f.post(c, "/jobs/"+id+"/cancel", url.Values{"csrf": {token}, "from": {"job"}}); got != http.StatusSeeOther {
		t.Errorf("plain cancel = %d", got)
	}
}

func TestQueueFiltersAndPages(t *testing.T) {
	f := newFixture(t)
	a, b := f.queued("show/a.mkv"), f.queued("film/b.mkv")
	if err := f.store.Cancel(b); err != nil {
		t.Fatal(err)
	}
	c := f.admin()
	_, body := f.get(c, "/queue?state=queued")
	if !strings.Contains(body, a) || strings.Contains(body, b) {
		t.Errorf("state filter: %s", body)
	}
	if !strings.Contains(body, `aria-current="page">queued`) {
		t.Error("the queued tab must be current")
	}
	_, body = f.get(c, "/fragments/queue?q=film")
	if strings.Contains(body, a) || !strings.Contains(body, b) {
		t.Errorf("search: %s", body)
	}
	_, body = f.get(c, "/queue?node=worker9")
	if !strings.Contains(body, "No jobs match this filter") || !strings.Contains(body, "node: worker9") {
		t.Errorf("node filter: %s", body)
	}
	// Typing in the search box updates the address bar.
	resp, _ := f.hx(c, http.MethodGet, "/fragments/queue?q=film&state=cancelled", nil, "/queue")
	if resp.Header.Get("HX-Push-Url") != "" {
		t.Error("a poll must not change the address")
	}
	req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/fragments/queue?q=film", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Trigger-Name", "q")
	r2, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.Header.Get("HX-Push-Url") != "/queue?q=film" {
		t.Errorf("push url = %q", r2.Header.Get("HX-Push-Url"))
	}
	// A third page past the end shows nothing rather than failing.
	if got, _ := f.get(c, "/queue?page=3"); got != http.StatusOK {
		t.Errorf("late page = %d", got)
	}
}

func TestLibraryHTMXActionsAndSearch(t *testing.T) {
	f := newFixture(t)
	f.add("show/a.mkv")
	f.add("film/b.mkv")
	c := f.admin()
	token := f.csrf(c)
	_, body := f.hx(c, http.MethodPost, "/library/add", url.Values{"csrf": {token}}, "/library")
	if !strings.Contains(body, "Select at least one title.") || !strings.Contains(body, "toast-warn") {
		t.Errorf("empty selection: %s", body)
	}
	_, body = f.hx(c, http.MethodPost, "/library/add", url.Values{"csrf": {token}, "path": {"film/b.mkv"}}, "/library?q=film")
	if !strings.Contains(body, `id="library-body"`) || !strings.Contains(body, "1 queued.") || strings.Contains(body, "show/") {
		t.Errorf("add keeps the search and reports: %s", body)
	}
	resp, body := f.hx(c, http.MethodGet, "/fragments/library?q=show", nil, "/library")
	if !strings.Contains(body, "a.mkv") || strings.Contains(body, "b.mkv") || resp.Header.Get("HX-Push-Url") != "/library?q=show" {
		t.Errorf("live search = %q: %s", resp.Header.Get("HX-Push-Url"), body)
	}
}

func TestBackfillAndScanFromTheWeb(t *testing.T) {
	f := newFixture(t)
	f.add("a.mkv")
	c := f.admin()
	token := f.csrf(c)
	if got, _ := f.post(c, "/backfill/start", url.Values{}); got != http.StatusForbidden {
		t.Errorf("backfill without CSRF = %d", got)
	}
	if got, _ := f.post(c, "/scan", url.Values{}); got != http.StatusForbidden {
		t.Errorf("scan without CSRF = %d", got)
	}
	_, body := f.hx(c, http.MethodPost, "/backfill/start", url.Values{"csrf": {token}}, "/")
	if st, _ := f.svc.BackfillStatus(); !st.Active || !strings.Contains(body, "Backfill started") || !strings.Contains(body, "Pause") {
		t.Errorf("start: active %v: %s", st.Active, body)
	}
	if got, _ := f.post(c, "/backfill/pause", url.Values{"csrf": {token}}); got != http.StatusSeeOther {
		t.Errorf("plain pause = %d", got)
	}
	if st, _ := f.svc.BackfillStatus(); st.Active {
		t.Error("pause did not stop backfill")
	}
	f.cfg.Mode = config.ModeManual
	_, body = f.hx(c, http.MethodPost, "/backfill/start", url.Values{"csrf": {token}}, "/")
	if st, _ := f.svc.BackfillStatus(); st.Active || !strings.Contains(body, "toast-error") || !strings.Contains(body, "auto mode") {
		t.Errorf("manual start must be refused: %s", body)
	}
	_, body = f.hx(c, http.MethodPost, "/scan", url.Values{"csrf": {token}}, "/")
	if !strings.Contains(body, "Scan requested") {
		t.Errorf("scan: %s", body)
	}
	resp, err := c.PostForm(f.server.URL+"/scan", url.Values{"csrf": {token}, "next": {"/library"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Location") != "/library" {
		t.Errorf("scan from the library returns to %q", resp.Header.Get("Location"))
	}
	resp, err = c.PostForm(f.server.URL+"/scan", url.Values{"csrf": {token}, "next": {"https://elsewhere.example"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Location") != "/" {
		t.Errorf("an outside next must be ignored: %q", resp.Header.Get("Location"))
	}
}

var nodeToken = regexp.MustCompile(`NODE_TOKEN: ([0-9a-f]+)`)

func TestAddRotateAndRemoveNodesFromTheWeb(t *testing.T) {
	f := newFixture(t)
	f.cfg.ControlURL = "https://control.example.org/jf"
	c := f.admin()
	token := f.csrf(c)

	code, body := f.post(c, "/nodes/add", url.Values{"csrf": {token}, "kind": {"worker"}, "name": {"worker2"}})
	if code != http.StatusOK || !strings.Contains(body, "ORCHESTRATOR_URL: https://control.example.org/jf") {
		t.Fatalf("add worker = %d: %s", code, body)
	}
	m := nodeToken.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no token shown: %s", body)
	}
	if n := f.svc.Nodes.ByToken(m[1]); n == nil || n.Name != "worker2" || n.Edge() {
		t.Errorf("the shown token must work: %+v", n)
	}

	code, body = f.post(c, "/nodes/add", url.Values{"csrf": {token}, "kind": {"edge"}, "name": {"edge9"},
		"url": {"https://edge9.example.com/"}, "quota_gb": {"50"}})
	if code != http.StatusOK || !strings.Contains(body, "EDGE_SIGNING_KEY: ") {
		t.Fatalf("add edge = %d: %s", code, body)
	}
	if n := f.svc.Nodes.Edge("edge9"); n == nil || n.URL != "https://edge9.example.com" || n.QuotaBytes != 50e9 {
		t.Errorf("edge = %+v", n)
	}

	for _, bad := range []url.Values{
		{"kind": {"edge"}, "name": {"edge8"}, "url": {"http://edge8.example.com"}, "quota_gb": {"5"}},
		{"kind": {"edge"}, "name": {"edge8"}, "url": {"https://edge8.example.com"}, "quota_gb": {"lots"}},
		{"kind": {"worker"}, "name": {"Bad Name"}},
		{"kind": {"worker"}, "name": {"worker2"}},
	} {
		bad.Set("csrf", token)
		if got, _ := f.post(c, "/nodes/add", bad); got != http.StatusSeeOther {
			t.Errorf("bad node %v = %d", bad, got)
		}
	}
	if _, body := f.get(c, "/nodes"); !strings.Contains(body, "flash-error") {
		t.Error("a refused node must say why")
	}

	code, body = f.post(c, "/nodes/worker2/rotate", url.Values{"csrf": {token}})
	m2 := nodeToken.FindStringSubmatch(body)
	if code != http.StatusOK || m2 == nil || m2[1] == m[1] || strings.Contains(body, "ORCHESTRATOR_URL") {
		t.Fatalf("rotate = %d: %s", code, body)
	}
	if f.svc.Nodes.ByToken(m[1]) != nil || f.svc.Nodes.ByToken(m2[1]) == nil {
		t.Error("rotate must replace the token")
	}

	if got, _ := f.post(c, "/nodes/worker2/delete", url.Values{"csrf": {token}}); got != http.StatusSeeOther {
		t.Errorf("delete = %d", got)
	}
	if f.svc.Nodes.ByToken(m2[1]) != nil {
		t.Error("a removed node's token must stop working")
	}
	if got, _ := f.post(c, "/nodes/nope/delete", url.Values{"csrf": {token}}); got != http.StatusSeeOther {
		t.Errorf("delete unknown = %d", got)
	}
}

func TestSecretPageWarnsWithoutControlURL(t *testing.T) {
	f := newFixture(t)
	c := f.admin()
	resp, err := c.PostForm(f.server.URL+"/nodes/add", url.Values{"csrf": {f.csrf(c)}, "kind": {"worker"}, "name": {"worker3"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if !strings.Contains(body, "CONTROL_URL is not set") || !strings.Contains(body, "ORCHESTRATOR_URL: https://control.example.com") {
		t.Errorf("placeholder: %s", body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("a page with a secret must not be cached")
	}
}

func TestJobPagePollsUntilTheJobEnds(t *testing.T) {
	f := newFixture(t)
	id := f.queued("a.mkv")
	c := f.admin()
	if _, body := f.get(c, "/jobs/"+id); !strings.Contains(body, `hx-get="/fragments/jobs/`+id+`"`) {
		t.Errorf("a queued job must poll: %s", body)
	}
	j, err := f.store.Claim("worker1", 60e9)
	if err != nil || j == nil {
		t.Fatalf("claim = %v, %v", j, err)
	}
	if _, err := f.store.Fail(j.ID, j.LeaseID, "boom", "frame=1\nError while decoding\nmore", 1); err != nil {
		t.Fatal(err)
	}
	_, body := f.get(c, "/fragments/jobs/"+id)
	if strings.Contains(body, "hx-trigger") || !strings.Contains(body, `<span class="bad">Error while decoding</span>`) {
		t.Errorf("a failed job must stop polling and mark bad lines: %s", body)
	}
	if !strings.Contains(body, "/jobs/"+id+"/log") {
		t.Error("no download link")
	}
	resp, err := c.Get(f.server.URL + "/jobs/" + id + "/log")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" || resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment;") || !strings.Contains(string(b), "Error while decoding") {
		t.Errorf("log download = %v: %q", resp.Header, b)
	}
	if got, _ := f.get(c, "/jobs/nope/log"); got != http.StatusNotFound {
		t.Errorf("unknown log = %d", got)
	}
	if j, _ := f.store.Get(id); j.State != model.StateFailed {
		t.Errorf("state = %s", j.State)
	}
}

// The CSP forbids inline styles, scripts and handlers, so a template that used
// one would break silently in the browser. Catch it here instead.
func TestTemplatesNeedNoInlineCode(t *testing.T) {
	inline := regexp.MustCompile(`(?i)\sstyle=|\son[a-z]+=|<script>|<script\s[^>]*>[^<]|javascript:|hx-on`)
	entries, err := assets.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := assets.ReadFile("templates/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if m := inline.Find(b); m != nil {
			t.Errorf("%s uses inline code the CSP blocks: %q", e.Name(), m)
		}
	}
}
