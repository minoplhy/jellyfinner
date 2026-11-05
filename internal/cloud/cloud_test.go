package cloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRegistryIsAlphabeticalAndChecksSettings(t *testing.T) {
	if got := Names(); !reflect.DeepEqual(got, []string{"cherry", "hetzner"}) {
		t.Errorf("Names() = %v", got)
	}
	if _, err := New("nope", Settings{Token: "x"}); err == nil {
		t.Error("an unknown provider must be refused")
	}
	for _, name := range Names() {
		if _, err := New(name, Settings{Types: []string{"a"}, Locations: []string{"b"}}); err == nil {
			t.Errorf("%s without a token must be refused", name)
		}
		if _, err := New(name, Settings{Token: "x", Locations: []string{"b"}}); err == nil {
			t.Errorf("%s without types must be refused", name)
		}
	}
	if _, err := New("cherry", Settings{Token: "x", Types: []string{"a"}, Locations: []string{"b"}, Image: "i"}); err == nil {
		t.Error("cherry without a project must be refused")
	}
	if _, err := New("cherry", Settings{Token: "x", Types: []string{"a"}, Locations: []string{"b"}, Project: "1"}); err == nil {
		t.Error("cherry without an image must be refused")
	}
	if EnvName("hetzner") != "CLOUD_HETZNER" {
		t.Error(EnvName("hetzner"))
	}
}

func TestBilling(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	for _, tt := range []struct {
		d    time.Duration
		want int64
	}{{0, 1}, {time.Minute, 1}, {time.Hour, 1}, {time.Hour + time.Second, 2}, {150 * time.Minute, 3}} {
		if got := BilledSteps(t0, t0.Add(tt.d), time.Hour); got != tt.want {
			t.Errorf("BilledSteps(%v) = %d, want %d", tt.d, got, tt.want)
		}
	}
	if got := PaidUntil(t0, t0.Add(10*time.Minute), time.Hour); !got.Equal(t0.Add(time.Hour)) {
		t.Errorf("PaidUntil = %v", got)
	}
	if got := Cost(t0, t0.Add(61*time.Minute), time.Hour, 0.5); got != 1.0 {
		t.Errorf("Cost = %v", got)
	}
	// Hours that started in a window: steps at 0h, 1h, 2h; window [1h, 2h).
	if got := HoursStarted(t0, t0.Add(150*time.Minute), time.Hour, t0.Add(time.Hour), t0.Add(2*time.Hour)); got != 1 {
		t.Errorf("HoursStarted = %v", got)
	}
}

func TestUserData(t *testing.T) {
	token := strings.Repeat("ab", 32)
	got, err := UserData(BootValues{Image: "ghcr.io/x/jellyfinner/worker:v1.2", URL: "https://control.example.com/jf", Token: token})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"x-image: &image ghcr.io/x/jellyfinner/worker:v1.2", `ORCHESTRATOR_URL: "https://control.example.com/jf"`,
		`NODE_TOKEN: "` + token + `"`, "docker compose up -d", "#!/bin/sh"} {
		if !strings.Contains(got, want) {
			t.Errorf("user data missing %q", want)
		}
	}
	for _, bad := range []BootValues{
		{Image: "x", URL: "http://plain.example.com", Token: token},
		{Image: "x", URL: "https://a.example.com\nJELLYFINNER_COMPOSE", Token: token},
		{Image: "x; rm -rf /", URL: "https://a.example.com", Token: token},
		{Image: "x", URL: "https://a.example.com", Token: "not hex"},
	} {
		if _, err := UserData(bad); err == nil {
			t.Errorf("UserData(%+v) must fail", bad)
		}
	}
}

// The boot script repeats the worker compose file, so it must keep its
// security settings.
func TestBootScriptMatchesWorkerCompose(t *testing.T) {
	compose, err := os.ReadFile("../../deploy/docker/compose.worker.yml")
	if err != nil {
		t.Skip("compose file not found:", err)
	}
	for _, line := range []string{`x-owner: &owner "10001:10001"`, "read_only: true", "no-new-privileges:true",
		`command: ["prepare", "--owner", *owner, "/scratch:750"]`, "cap_add: [CHOWN, DAC_OVERRIDE, FOWNER]", `test: ["CMD", "worker", "health"]`} {
		if !strings.Contains(string(compose), line) {
			t.Fatalf("compose.worker.yml no longer has %q: update this test and the boot script together", line)
		}
		if !strings.Contains(bootScript, line) {
			t.Errorf("boot script lacks %q from compose.worker.yml", line)
		}
	}
}

// fakeAPI records requests and answers from a handler.
type fakeAPI struct {
	mu       sync.Mutex
	requests []string
	bodies   map[string]json.RawMessage
	srv      *httptest.Server
}

func newFake(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *fakeAPI {
	f := &fakeAPI{bodies: map[string]json.RawMessage{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
		if len(body) > 0 {
			f.bodies[r.Method+" "+r.URL.Path] = body
		}
		f.mu.Unlock()
		handle(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func quick(p Provider) Provider {
	switch v := p.(type) {
	case *hetzner:
		v.api.wait = time.Millisecond
	case *cherry:
		v.api.wait = time.Millisecond
	}
	return p
}

func TestHetzner(t *testing.T) {
	var mu sync.Mutex
	exists := map[string]bool{}
	ipExists := map[string]bool{}
	createStatus := http.StatusCreated
	createCode := ""
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/servers":
			if createStatus != http.StatusCreated {
				w.WriteHeader(createStatus)
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": createCode, "message": "no"}})
				return
			}
			exists["7"], ipExists["70"] = true, true
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"server":{"id":7,"name":"cloud-hetzner-a","created":"2026-11-05T10:00:00Z",
				"labels":{"jellyfinner-cluster":"c1"},"server_type":{"name":"cx"},"location":{"name":"l1"},
				"public_net":{"ipv4":{"id":70},"ipv6":null}}}`)
		case r.URL.Path == "/servers/7" && r.Method == http.MethodGet:
			if !exists["7"] {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"error":{"code":"not_found","message":"gone"}}`)
				return
			}
			io.WriteString(w, `{"server":{"id":7,"labels":{"jellyfinner-cluster":"c1"}}}`)
		case r.URL.Path == "/servers/8" && r.Method == http.MethodGet:
			io.WriteString(w, `{"server":{"id":8,"labels":{"jellyfinner-cluster":"other"}}}`)
		case r.URL.Path == "/servers/7" && r.Method == http.MethodDelete:
			delete(exists, "7") // the address stays, as if something kept it
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/primary_ips/70":
			if r.Method == http.MethodDelete {
				delete(ipExists, "70")
			}
			if !ipExists["70"] {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			io.WriteString(w, `{}`)
		case r.URL.Path == "/servers" && r.Method == http.MethodGet:
			io.WriteString(w, `{"servers":[{"id":7,"labels":{"jellyfinner-cluster":"c1"}},{"id":9,"labels":{"jellyfinner-cluster":"zz"}}],
				"meta":{"pagination":{"next_page":null}}}`)
		case r.URL.Path == "/pricing":
			io.WriteString(w, `{"pricing":{"currency":"EUR","primary_ips":[{"type":"ipv4","prices":[{"location":"l1","price_hourly":{"net":"0.001","gross":"0.0012"}}]},
				{"type":"ipv6","prices":[{"location":"l1","price_hourly":{"net":"9","gross":"9"}}]}]}}`)
		case r.URL.Path == "/server_types":
			io.WriteString(w, `{"server_types":[{"name":"cx","prices":[{"location":"l1","price_hourly":{"net":"0.0100","gross":"0.0119"}},
				{"location":"other","price_hourly":{"net":"5","gross":"5"}}]}]}`)
		case r.URL.Path == "/locations":
			if r.URL.Query().Get("name") == "l1" {
				io.WriteString(w, `{"locations":[{}]}`)
				return
			}
			io.WriteString(w, `{"locations":[]}`)
		case r.URL.Path == "/images":
			io.WriteString(w, `{"images":[{}]}`)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	})
	p, err := New("hetzner", Settings{Token: "tok", Types: []string{"cx"}, Locations: []string{"l1"}, BaseURL: f.srv.URL, SSHKey: "me"})
	if err != nil {
		t.Fatal(err)
	}
	quick(p)
	ctx := context.Background()

	srv, err := p.Create(ctx, Spec{Name: "cloud-hetzner-a", Type: "cx", Location: "l1", UserData: "#!/bin/sh", Cluster: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if srv.ID != "7" || srv.Resources != "70" || srv.Cluster != "c1" || srv.Type != "cx" {
		t.Errorf("created = %+v", srv)
	}
	var body map[string]any
	json.Unmarshal(f.bodies["POST /servers"], &body)
	labels, _ := body["labels"].(map[string]any)
	if body["image"] != "docker-ce" || body["user_data"] != "#!/bin/sh" || labels[LabelCluster] != "c1" || labels[LabelRole] != RoleWorker {
		t.Errorf("create body = %v", body)
	}

	if err := p.Delete(ctx, Server{ID: "8"}, "c1"); !errors.Is(err, ErrForeign) {
		t.Errorf("deleting another cluster's server: %v", err)
	}
	if gone, err := p.Gone(ctx, srv); err != nil || gone {
		t.Fatalf("Gone before delete = %v, %v", gone, err)
	}
	if err := p.Delete(ctx, srv, "c1"); err != nil {
		t.Fatal(err)
	}
	// The server is gone but its address is not, and that is still billed.
	if gone, err := p.Gone(ctx, srv); err != nil || gone {
		t.Fatalf("Gone with a leftover address = %v, %v", gone, err)
	}
	if err := p.Delete(ctx, srv, "c1"); err != nil {
		t.Fatal(err)
	}
	if gone, err := p.Gone(ctx, srv); err != nil || !gone {
		t.Fatalf("Gone after cleanup = %v, %v", gone, err)
	}
	if err := p.Delete(ctx, srv, "c1"); err != nil {
		t.Errorf("deleting what is gone must succeed: %v", err)
	}

	list, err := p.List(ctx, "c1")
	if err != nil || len(list) != 1 || list[0].ID != "7" {
		t.Errorf("List = %+v, %v", list, err)
	}
	prices, err := p.Prices(ctx)
	if err != nil || len(prices) != 1 {
		t.Fatalf("Prices = %+v, %v", prices, err)
	}
	if pr := prices[0]; pr.Type != "cx" || pr.Location != "l1" || pr.Currency != "EUR" || pr.Hourly < 0.0109 || pr.Hourly > 0.0111 || pr.Taxed {
		t.Errorf("price = %+v, want net server plus IPv4", pr)
	}
	if err := p.Check(ctx); err != nil {
		t.Errorf("Check = %v", err)
	}

	for _, tt := range []struct {
		status int
		code   string
		want   error
	}{
		{http.StatusUnprocessableEntity, "resource_unavailable", ErrUnavailable},
		{http.StatusForbidden, "resource_limit_exceeded", ErrUnavailable},
		{http.StatusUnprocessableEntity, "resource_limit_exceeded", ErrUnavailable},
		{http.StatusBadRequest, "invalid_input", ErrConfig},
		{http.StatusTooManyRequests, "rate_limit_exceeded", ErrUnavailable},
		{http.StatusServiceUnavailable, "", ErrUnavailable},
	} {
		mu.Lock()
		createStatus, createCode = tt.status, tt.code
		mu.Unlock()
		if _, err := p.Create(ctx, Spec{Name: "x", Type: "cx", Location: "l1", Cluster: "c1"}); !errors.Is(err, tt.want) {
			t.Errorf("create %d %s: %v, want %v", tt.status, tt.code, err, tt.want)
		}
	}
	// A failed create is sent once, except a 429, which never created anything.
	if n := f.count("POST /servers"); n != 1+5+4 {
		t.Errorf("creates sent = %d", n)
	}

	bad, _ := New("hetzner", Settings{Token: "tok", Types: []string{"cx"}, Locations: []string{"nowhere"}, BaseURL: f.srv.URL})
	if err := bad.Check(ctx); err == nil || !strings.Contains(err.Error(), "nowhere") {
		t.Errorf("Check with a bad location = %v", err)
	}
	wrong, _ := New("hetzner", Settings{Token: "nope", Types: []string{"cx"}, Locations: []string{"l1"}, BaseURL: f.srv.URL})
	if _, err := wrong.Create(ctx, Spec{Name: "x", Type: "cx", Location: "l1", Cluster: "c1"}); !errors.Is(err, ErrConfig) {
		t.Errorf("a bad token must be a configuration error: %v", err)
	}
}

func TestCherry(t *testing.T) {
	var mu sync.Mutex
	exists := map[string]bool{}
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/projects/42/servers":
			exists["5"] = true
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":5,"hostname":"cloud-cherry-a","tags":{"jellyfinner-cluster":"c1"},"created_at":"2026-11-05T10:00:00Z",
				"region":{"slug":"r1"},"plan":{"slug":"vps_1"}}`)
		case r.URL.Path == "/servers/5" && r.Method == http.MethodGet:
			if !exists["5"] {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"code":404,"message":"not found"}`)
				return
			}
			io.WriteString(w, `{"id":5,"tags":{"jellyfinner-cluster":"c1"}}`)
		case r.URL.Path == "/servers/6" && r.Method == http.MethodGet:
			io.WriteString(w, `{"id":6,"tags":{}}`)
		case r.URL.Path == "/servers/5" && r.Method == http.MethodDelete:
			delete(exists, "5")
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/projects/42/servers" && r.Method == http.MethodGet:
			// Two pages: a full one, then a short one.
			var page []map[string]any
			if r.URL.Query().Get("offset") == "0" {
				for i := range 100 {
					tags := map[string]string{}
					if i == 3 {
						tags[LabelCluster] = "c1"
					}
					page = append(page, map[string]any{"id": i + 100, "tags": tags})
				}
			} else {
				page = append(page, map[string]any{"id": 1, "tags": map[string]string{LabelCluster: "c1"}})
			}
			json.NewEncoder(w).Encode(page)
		case r.URL.Path == "/projects/42":
			io.WriteString(w, `{"id":42}`)
		case r.URL.Path == "/plans/vps_1":
			io.WriteString(w, `{"slug":"vps_1","pricing":[{"price":9,"currency":"EUR","taxed":false,"unit":"Monthly"},
				{"price":0.015,"currency":"EUR","taxed":false,"unit":"Hourly"}],"available_regions":[{"slug":"r1"}]}`)
		case r.URL.Path == "/plans/vps_1/images":
			io.WriteString(w, `[{"slug":"ubuntu_24_04_64bit"}]`)
		case r.URL.Path == "/plans/nope", strings.HasPrefix(r.URL.Path, "/projects/999"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	})
	settings := Settings{Token: "tok", Project: "42", Types: []string{"vps_1"}, Locations: []string{"r1"},
		Image: "ubuntu_24_04_64bit", BaseURL: f.srv.URL, SSHKey: "12"}
	p, err := New("cherry", settings)
	if err != nil {
		t.Fatal(err)
	}
	quick(p)
	ctx := context.Background()

	srv, err := p.Create(ctx, Spec{Name: "cloud-cherry-a", Type: "vps_1", Location: "r1", UserData: "#!/bin/sh\necho hi", Cluster: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if srv.ID != "5" || srv.Type != "vps_1" || srv.Location != "r1" || srv.Cluster != "c1" || srv.Created.IsZero() {
		t.Errorf("created = %+v", srv)
	}
	var body map[string]any
	json.Unmarshal(f.bodies["POST /projects/42/servers"], &body)
	decoded, _ := base64.StdEncoding.DecodeString(body["user_data"].(string))
	tags, _ := body["tags"].(map[string]any)
	if string(decoded) != "#!/bin/sh\necho hi" || body["cycle"] != "hourly" || body["spot_market"] != false ||
		tags[LabelCluster] != "c1" || body["hostname"] != "cloud-cherry-a" || !reflect.DeepEqual(body["ssh_keys"], []any{12.0}) {
		t.Errorf("create body = %v", body)
	}

	if err := p.Delete(ctx, Server{ID: "6"}, "c1"); !errors.Is(err, ErrForeign) {
		t.Errorf("deleting an untagged server: %v", err)
	}
	if err := p.Delete(ctx, srv, "c1"); err != nil {
		t.Fatal(err)
	}
	if gone, err := p.Gone(ctx, srv); err != nil || !gone {
		t.Errorf("Gone = %v, %v", gone, err)
	}
	if err := p.Delete(ctx, srv, "c1"); err != nil {
		t.Errorf("deleting what is gone must succeed: %v", err)
	}

	list, err := p.List(ctx, "c1")
	if err != nil || len(list) != 2 {
		t.Errorf("List = %+v, %v", list, err)
	}
	prices, err := p.Prices(ctx)
	if err != nil || len(prices) != 1 || prices[0].Hourly != 0.015 || prices[0].Currency != "EUR" || prices[0].Location != "r1" {
		t.Errorf("Prices = %+v, %v", prices, err)
	}
	if err := p.Check(ctx); err != nil {
		t.Errorf("Check = %v", err)
	}
	settings.Types = []string{"nope"}
	bad, _ := New("cherry", settings)
	if err := bad.Check(ctx); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("Check with an unknown plan = %v", err)
	}
	settings.Types, settings.Project = []string{"vps_1"}, "999"
	missing, _ := New("cherry", settings)
	if _, err := missing.Create(ctx, Spec{Name: "x", Type: "vps_1", Location: "r1", Cluster: "c1"}); !errors.Is(err, ErrConfig) {
		t.Errorf("an unknown project must be a configuration error: %v", err)
	}
}
