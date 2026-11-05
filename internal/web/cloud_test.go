package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"jellyfinner/internal/cloud"
	"jellyfinner/internal/config"
	"jellyfinner/internal/orchestrator"
)

// stubProvider prices one option and creates servers that exist forever.
type stubProvider struct{ name string }

func (p stubProvider) Name() string               { return p.name }
func (p stubProvider) BillingStep() time.Duration { return time.Hour }
func (p stubProvider) Billing() cloud.Billing {
	return cloud.Billing{Note: p.name + " rounds up to whole hours.", Checked: "2026-11-05",
		Sources: []cloud.Source{{Title: p.name + " billing FAQ", URL: "https://" + p.name + ".example/faq"}}}
}
func (p stubProvider) Prices(context.Context) ([]cloud.Price, error) {
	return []cloud.Price{{Type: "t1", Location: "l1", Hourly: 0.05, Currency: "EUR"}}, nil
}
func (p stubProvider) Create(_ context.Context, s cloud.Spec) (cloud.Server, error) {
	return cloud.Server{ID: "1", Name: s.Name, Type: s.Type, Location: s.Location, Cluster: s.Cluster}, nil
}
func (p stubProvider) Delete(context.Context, cloud.Server, string) error { return nil }
func (p stubProvider) Gone(context.Context, cloud.Server) (bool, error)   { return false, nil }
func (p stubProvider) List(context.Context, string) ([]cloud.Server, error) {
	return nil, nil
}
func (p stubProvider) Check(context.Context) error { return nil }

func TestCloudSection(t *testing.T) {
	f := newFixture(t)
	c := f.admin()
	if _, body := f.get(c, "/nodes"); strings.Contains(body, "Cloud workers") {
		t.Fatal("no providers listed, so no cloud section")
	}

	f.cfg.ControlURL = "https://control.example.com"
	f.cfg.Cloud = config.Cloud{Max: 1, Wait: time.Minute, QueueThreshold: 1, Idle: time.Minute, BootTimeout: time.Hour,
		MaxLifetime: time.Hour, DeleteTimeout: time.Hour, MaxHoursPerDay: 8, WorkerImage: config.DefaultWorkerImage,
		Providers: []config.CloudProvider{{Name: "zeta", Types: []string{"t1"}, Locations: []string{"l1"}},
			{Name: "alpha", Types: []string{"t1"}, Locations: []string{"l1"}}}}
	cl, err := orchestrator.NewCloud(f.svc, []cloud.Provider{stubProvider{"zeta"}, stubProvider{"alpha"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.svc.Cloud = cl
	srv, err := cl.Start(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	_, body := f.get(c, "/nodes")
	for _, want := range []string{"Cloud workers", "zeta</b>", "<b>zeta, then alpha</b>", srv.Name, "0.0500 EUR",
		"zeta rounds up to whole hours.", "https://zeta.example/faq", "checked 2026-11-05", "excluding VAT", "Paid until",
		"/cloud/servers/" + srv.Name + "/drain", `href="/usage"`} {
		if !strings.Contains(body, want) {
			t.Errorf("Nodes page lacks %q", want)
		}
	}
	// Estimates and history live on the Usage page alone.
	if strings.Contains(body, "Estimated cost by option") || strings.Contains(body, "this month") {
		t.Error("Nodes page still shows estimates or monthly spending")
	}
	_, body = f.get(c, "/usage")
	for _, want := range []string{"Cloud spending", "zeta</td><td>t1</td>", "zeta t1 in l1, 0.0500 EUR an hour", "alpha t1 in l1",
		"no jobs yet", "zeta rounds up to whole hours.", "excluding VAT", "prices never change it"} {
		if !strings.Contains(body, want) {
			t.Errorf("Usage page lacks %q", want)
		}
	}
	if strings.Index(body, "zeta t1 in l1") > strings.Index(body, "alpha t1 in l1") {
		t.Error("forecast rows must follow CLOUD_PROVIDERS order")
	}

	token := f.csrf(c)
	if code, _ := f.post(c, "/cloud/pause", url.Values{"csrf": {token}}); code != http.StatusSeeOther {
		t.Fatalf("pause = %d", code)
	}
	if paused, _ := cl.Paused(); !paused {
		t.Error("cloud not paused")
	}
	if _, body := f.get(c, "/nodes"); !strings.Contains(body, "paused: no new servers") {
		t.Error("the page must show the pause")
	}
	if code, _ := f.post(c, "/cloud/resume", url.Values{"csrf": {token}}); code != http.StatusSeeOther {
		t.Fatalf("resume = %d", code)
	}
	if code, _ := f.post(c, "/cloud/servers/"+srv.Name+"/drain", url.Values{"csrf": {token}}); code != http.StatusSeeOther {
		t.Fatalf("drain = %d", code)
	}
	views, _ := cl.Servers()
	if len(views) != 1 || views[0].State != "draining" {
		t.Errorf("after drain: %+v", views)
	}
	if code, body := f.get(c, "/fragments/nodes/cloud"); code != http.StatusOK || !strings.Contains(body, `id="cloud"`) {
		t.Errorf("cloud fragment = %d", code)
	}
}
