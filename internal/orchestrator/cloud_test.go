package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyfinner/internal/cloud"
	"jellyfinner/internal/config"
	"jellyfinner/internal/model"
)

// fakeProvider is an in-memory cloud.
type fakeProvider struct {
	name string

	mu        sync.Mutex
	servers   map[string]cloud.Server
	next      int
	createErr map[string]error // by type/location
	creates   []string
	deletes   int
	stubborn  int // Gone answers false this many more times after a delete
	prices    []cloud.Price
	priceErr  error
	priceHits int
}

func newFakeProvider(name string) *fakeProvider {
	return &fakeProvider{name: name, servers: map[string]cloud.Server{}, createErr: map[string]error{}}
}

func (p *fakeProvider) Name() string               { return p.name }
func (p *fakeProvider) BillingStep() time.Duration { return time.Hour }
func (p *fakeProvider) Billing() cloud.Billing {
	return cloud.Billing{Note: p.name + " bills hourly", Sources: []cloud.Source{{Title: p.name, URL: "https://" + p.name + ".example/billing"}}}
}
func (p *fakeProvider) Check(context.Context) error { return nil }

func (p *fakeProvider) Prices(context.Context) ([]cloud.Price, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.priceHits++
	return p.prices, p.priceErr
}

func (p *fakeProvider) Create(_ context.Context, s cloud.Spec) (cloud.Server, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creates = append(p.creates, s.Type+"/"+s.Location)
	if err := p.createErr[s.Type+"/"+s.Location]; err != nil {
		return cloud.Server{}, err
	}
	if !strings.Contains(s.UserData, "NODE_TOKEN") || s.Cluster == "" {
		return cloud.Server{}, errors.New("spec lacks user data or cluster")
	}
	p.next++
	srv := cloud.Server{ID: fmt.Sprint(p.next), Name: s.Name, Type: s.Type, Location: s.Location, Cluster: s.Cluster}
	p.servers[srv.ID] = srv
	return srv, nil
}

func (p *fakeProvider) Delete(_ context.Context, s cloud.Server, cluster string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	live, ok := p.servers[s.ID]
	if !ok {
		return nil
	}
	if live.Cluster != cluster {
		return cloud.ErrForeign
	}
	p.deletes++
	delete(p.servers, s.ID)
	return nil
}

func (p *fakeProvider) Gone(_ context.Context, s cloud.Server) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.servers[s.ID]; ok {
		return false, nil
	}
	if p.stubborn > 0 {
		p.stubborn--
		return false, nil
	}
	return true, nil
}

func (p *fakeProvider) List(_ context.Context, cluster string) ([]cloud.Server, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []cloud.Server
	for _, s := range p.servers {
		if s.Cluster == cluster {
			out = append(out, s)
		}
	}
	return out, nil
}

func (p *fakeProvider) count() (creates, deletes, live int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.creates), p.deletes, len(p.servers)
}

type cloudFixture struct {
	*fixture
	cloud *Cloud
	fakes map[string]*fakeProvider
	seen  map[string]time.Time
}

// newCloud lists fake providers in the given order, each with types t1,t2
// and location l1.
func newCloud(t *testing.T, order ...string) *cloudFixture {
	t.Helper()
	f := newFixture(t)
	f.cfg.ControlURL = "https://control.example.com"
	f.cfg.Cloud = config.Cloud{Max: 1, Wait: 10 * time.Minute, QueueThreshold: 3, Idle: 5 * time.Minute,
		BootTimeout: 10 * time.Minute, MaxLifetime: 12 * time.Hour, DeleteTimeout: 15 * time.Minute,
		MaxHoursPerDay: 8, WorkerImage: config.DefaultWorkerImage}
	cf := &cloudFixture{fixture: f, fakes: map[string]*fakeProvider{}, seen: map[string]time.Time{}}
	var providers []cloud.Provider
	for _, name := range order {
		f.cfg.Cloud.Providers = append(f.cfg.Cloud.Providers, config.CloudProvider{Name: name, Types: []string{"t1", "t2"}, Locations: []string{"l1"}})
		fp := newFakeProvider(name)
		cf.fakes[name] = fp
		providers = append(providers, fp)
	}
	c, err := NewCloud(f.svc, providers, func() map[string]time.Time { return cf.seen })
	if err != nil {
		t.Fatal(err)
	}
	f.svc.Cloud = c
	cf.cloud = c
	return cf
}

func (cf *cloudFixture) queue(id string, priority int) {
	cf.t.Helper()
	if _, err := cf.store.Enqueue(&model.Job{ID: id, Path: id + ".mkv", OutputPath: id + ".mp4", Priority: priority, SourceSize: 2e9}); err != nil {
		cf.t.Fatal(err)
	}
}

func (cf *cloudFixture) servers() []*model.CloudServer {
	cf.t.Helper()
	rows, err := cf.store.CloudServers(time.Unix(0, 0))
	if err != nil {
		cf.t.Fatal(err)
	}
	return rows
}

func (cf *cloudFixture) tick() { cf.cloud.Tick(context.Background()) }

func TestCloudFailoverFollowsTheOperatorsOrder(t *testing.T) {
	for _, order := range [][]string{{"alpha", "beta"}, {"beta", "alpha"}} {
		t.Run(strings.Join(order, ","), func(t *testing.T) {
			cf := newCloud(t, order...)
			first, second := cf.fakes[order[0]], cf.fakes[order[1]]
			first.createErr["t1/l1"] = fmt.Errorf("%w: sold out", cloud.ErrUnavailable)
			first.createErr["t2/l1"] = fmt.Errorf("%w: sold out", cloud.ErrUnavailable)

			r, err := cf.cloud.Start(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			if r.Provider != order[1] || r.Type != "t1" {
				t.Errorf("started on %s %s, want %s t1", r.Provider, r.Type, order[1])
			}
			if strings.Join(first.creates, ",") != "t1/l1,t2/l1" || len(second.creates) != 1 {
				t.Errorf("tried %v then %v", first.creates, second.creates)
			}
			// The failed options cool down, so a second try skips them.
			cf.cfg.Cloud.Max = 2
			cf.cloud.cfg.Max = 2
			if _, err := cf.cloud.Start(context.Background(), ""); err != nil {
				t.Fatal(err)
			}
			if len(first.creates) != 2 {
				t.Errorf("cooling options were tried again: %v", first.creates)
			}
		})
	}
}

func TestCloudEachProviderAlone(t *testing.T) {
	for _, name := range []string{"alpha", "beta"} {
		cf := newCloud(t, name)
		r, err := cf.cloud.Start(context.Background(), "")
		if err != nil || r.Provider != name {
			t.Fatalf("%s alone: %+v, %v", name, r, err)
		}
	}
	cf := newCloud(t, "alpha", "beta")
	r, err := cf.cloud.Start(context.Background(), "beta")
	if err != nil || r.Provider != "beta" {
		t.Errorf("--provider beta: %+v, %v", r, err)
	}
	if c, _, _ := cf.fakes["alpha"].count(); c != 0 {
		t.Error("a provider not asked for was contacted")
	}
	if _, err := cf.cloud.Start(context.Background(), "gamma"); err == nil {
		t.Error("an unlisted provider must be refused")
	}
}

func TestCloudOffWithNoProviders(t *testing.T) {
	cf := newCloud(t)
	cf.queue("a", model.PriorityNew)
	cf.now = cf.now.Add(time.Hour)
	cf.tick()
	if len(cf.servers()) != 0 {
		t.Error("no providers must mean no servers")
	}
	if _, err := cf.cloud.Start(context.Background(), ""); err == nil {
		t.Error("Start with no providers must fail")
	}
}

func TestCloudConfigErrorSetsProviderAside(t *testing.T) {
	cf := newCloud(t, "alpha", "beta")
	cf.fakes["alpha"].createErr["t1/l1"] = fmt.Errorf("%w: bad token", cloud.ErrConfig)
	r, err := cf.cloud.Start(context.Background(), "")
	if err != nil || r.Provider != "beta" {
		t.Fatalf("Start = %+v, %v", r, err)
	}
	if len(cf.fakes["alpha"].creates) != 1 {
		t.Errorf("a refused provider kept being tried: %v", cf.fakes["alpha"].creates)
	}
	if _, off := cf.cloud.Disabled()["alpha"]; !off {
		t.Error("alpha must be set aside")
	}
	if len(cf.notify.messages) == 0 {
		t.Error("setting a provider aside must notify")
	}
	// The failed create left no node behind.
	nodes, _ := cf.store.LoadNodes()
	if len(nodes) != 1 {
		t.Errorf("nodes = %+v", nodes)
	}
}

func TestCloudTriggers(t *testing.T) {
	cf := newCloud(t, "alpha")
	cf.queue("bf", model.PriorityBackfill)
	cf.queue("a", model.PriorityNew)
	cf.tick()
	if len(cf.servers()) != 0 {
		t.Fatal("started before CLOUD_WAIT with a short queue")
	}
	cf.now = cf.now.Add(10 * time.Minute)
	cf.tick()
	if len(cf.servers()) != 1 {
		t.Fatal("no server after CLOUD_WAIT")
	}
	cf.tick()
	if len(cf.servers()) != 1 {
		t.Error("a booting server covers the one waiting job")
	}

	// A long queue starts one before the wait.
	cf2 := newCloud(t, "alpha")
	for i := range 3 {
		cf2.queue(fmt.Sprint("q", i), model.PriorityNew)
	}
	cf2.tick()
	if len(cf2.servers()) != 1 {
		t.Error("CLOUD_QUEUE_THRESHOLD did not start a server")
	}

	// Backfill alone never starts one, unless asked for and running.
	cf3 := newCloud(t, "alpha")
	cf3.queue("bf", model.PriorityBackfill)
	if err := cf3.store.SetMeta(model.MetaBackfillActive, "1"); err != nil {
		t.Fatal(err)
	}
	cf3.now = cf3.now.Add(time.Hour)
	cf3.tick()
	if len(cf3.servers()) != 0 {
		t.Error("backfill started a server without CLOUD_BACKFILL")
	}
	cf3.cloud.cfg.Backfill = true
	cf3.tick()
	if len(cf3.servers()) != 1 {
		t.Error("CLOUD_BACKFILL=true must count backfill")
	}
}

func TestCloudPausedScalerStartsNothing(t *testing.T) {
	cf := newCloud(t, "alpha")
	if err := cf.cloud.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		cf.queue(fmt.Sprint("q", i), model.PriorityNew)
	}
	cf.now = cf.now.Add(time.Hour)
	cf.tick()
	if len(cf.servers()) != 0 {
		t.Error("a paused scaler started a server")
	}
	if err := cf.cloud.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	cf.tick()
	if len(cf.servers()) != 1 {
		t.Error("resuming must allow servers again")
	}
}

func TestCloudLimits(t *testing.T) {
	cf := newCloud(t, "alpha")
	if _, err := cf.cloud.Start(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := cf.cloud.Start(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "CLOUD_MAX") {
		t.Errorf("past CLOUD_MAX: %v", err)
	}

	cf2 := newCloud(t, "alpha")
	cf2.cloud.cfg.Max = 10
	cf2.cloud.cfg.MaxHoursPerDay = 2
	for range 2 {
		if _, err := cf2.cloud.Start(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cf2.cloud.Start(context.Background(), ""); !errors.Is(err, ErrCapped) {
		t.Errorf("past CLOUD_MAX_HOURS_PER_DAY: %v", err)
	}
}

func TestCloudSpendCap(t *testing.T) {
	cf := newCloud(t, "alpha", "beta")
	cf.cloud.cfg.Max = 10
	cf.cloud.cfg.MaxSpendPerDay = 0.25
	cf.fakes["alpha"].prices = []cloud.Price{{Type: "t1", Location: "l1", Hourly: 0.10, Currency: "EUR"}}
	for range 2 {
		if _, err := cf.cloud.Start(context.Background(), "alpha"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cf.cloud.Start(context.Background(), "alpha"); err == nil || !strings.Contains(err.Error(), "SPEND") {
		t.Errorf("past CLOUD_MAX_SPEND_PER_DAY: %v", err)
	}
	cf.fakes["beta"].prices = []cloud.Price{{Type: "t1", Location: "l1", Hourly: 0.01, Currency: "USD"}}
	if _, err := cf.cloud.Start(context.Background(), "beta"); err == nil || !strings.Contains(err.Error(), "one currency") {
		t.Errorf("mixed currencies with a spend cap: %v", err)
	}
}

func TestCloudLifecycle(t *testing.T) {
	cf := newCloud(t, "alpha")
	ctx := context.Background()
	fp := cf.fakes["alpha"]
	r, err := cf.cloud.Start(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	cf.queue("a", model.PriorityNew)

	// It calls in, takes a job and finishes it.
	cf.now = cf.now.Add(2 * time.Minute)
	cf.seen[r.Name] = cf.now
	cf.tick()
	if got := cf.servers()[0]; got.State != model.CloudReady || got.FirstSeen == 0 {
		t.Fatalf("after calling in: %+v", got)
	}
	j, err := cf.store.Claim(r.Name, time.Hour)
	if err != nil || j == nil {
		t.Fatalf("claim = %v, %v", j, err)
	}
	cf.now = cf.now.Add(30 * time.Minute)
	cf.tick()
	if got := cf.servers()[0]; got.IdleSince != 0 {
		t.Errorf("busy server marked idle: %+v", got)
	}
	if err := cf.store.Complete(j.ID, j.LeaseID, "{}", ""); err != nil {
		t.Fatal(err)
	}

	// Idle, but 28 minutes are still paid for, so it stays.
	cf.tick()
	cf.now = cf.now.Add(10 * time.Minute)
	cf.tick()
	if got := cf.servers()[0]; got.State != model.CloudReady {
		t.Fatalf("drained with paid time left: %+v", got)
	}

	// Near the end of the hour it drains and is deleted, but the provider
	// does not confirm at once.
	fp.stubborn = 2
	cf.now = time.Unix(r.Created, 0).Add(56 * time.Minute)
	cf.tick()
	got := cf.servers()[0]
	if got.State != model.CloudDeleting || got.DeleteAt == 0 {
		t.Fatalf("near the end of the hour: %+v", got)
	}
	if _, deletes, _ := fp.count(); deletes != 1 {
		t.Errorf("deletes = %d", deletes)
	}
	cf.now = cf.now.Add(time.Minute)
	cf.tick()
	if got := cf.servers()[0]; got.State != model.CloudDeleting {
		t.Fatalf("recorded deleted before the provider confirmed: %+v", got)
	}
	nodes, _ := cf.store.LoadNodes()
	if len(nodes) != 1 || !nodes[0].Paused {
		t.Errorf("node must stay, paused, until the delete is confirmed: %+v", nodes)
	}

	// Unconfirmed past the timeout: one alert.
	before := len(cf.notify.messages)
	fp.stubborn = 100
	cf.now = cf.now.Add(20 * time.Minute)
	cf.tick()
	cf.now = cf.now.Add(time.Minute)
	cf.tick()
	if alerts := len(cf.notify.messages) - before; alerts != 1 {
		t.Errorf("unconfirmed delete alerts = %d, want 1", alerts)
	}
	if !cf.servers()[0].Alerted || !cf.cloud.DeleteOverdue(cf.servers()[0]) {
		t.Error("the overdue delete must be recorded")
	}

	// A restart carries on, and confirmation finally removes the node.
	fp.stubborn = 0
	restarted, err := NewCloud(cf.svc, []cloud.Provider{fp}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cf.now = cf.now.Add(time.Minute)
	restarted.Tick(ctx)
	restarted.Tick(ctx)
	if got := cf.servers()[0]; got.State != model.CloudDeleted || got.Deleted == 0 {
		t.Fatalf("after confirmation: %+v", got)
	}
	if nodes, _ := cf.store.LoadNodes(); len(nodes) != 0 {
		t.Errorf("node not removed after deletion: %+v", nodes)
	}
}

func TestCloudBootTimeout(t *testing.T) {
	cf := newCloud(t, "alpha")
	if _, err := cf.cloud.Start(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	cf.now = cf.now.Add(11 * time.Minute)
	cf.tick()
	cf.tick()
	if got := cf.servers()[0]; got.State != model.CloudDeleted {
		t.Fatalf("a server that never called in: %+v", got)
	}
	if !cf.cloud.coolingDown("alpha", "t1", "l1", cf.now) {
		t.Error("its option must cool down")
	}
}

func TestCloudDrainWaitsForTheJob(t *testing.T) {
	cf := newCloud(t, "alpha")
	r, err := cf.cloud.Start(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	cf.queue("a", model.PriorityNew)
	cf.seen[r.Name] = cf.now
	cf.tick()
	j, _ := cf.store.Claim(r.Name, time.Hour)
	if j == nil {
		t.Fatal("no job")
	}
	if err := cf.cloud.Drain(r.Name); err != nil {
		t.Fatal(err)
	}
	cf.queue("b", model.PriorityNew)
	if j2, _ := cf.store.Claim(r.Name, time.Hour); j2 != nil {
		t.Error("a draining server claimed another job")
	}
	cf.tick()
	if _, deletes, _ := cf.fakes["alpha"].count(); deletes != 0 {
		t.Fatal("deleted with a job running")
	}
	if err := cf.store.Complete(j.ID, j.LeaseID, "{}", ""); err != nil {
		t.Fatal(err)
	}
	cf.tick()
	cf.tick()
	if got := cf.servers()[0]; got.State != model.CloudDeleted {
		t.Errorf("after the job: %+v", got)
	}
}

func TestCloudPausingACloudNodeDrainsIt(t *testing.T) {
	cf := newCloud(t, "alpha")
	r, _ := cf.cloud.Start(context.Background(), "")
	cf.seen[r.Name] = cf.now
	cf.tick()
	if err := cf.svc.PauseNode(r.Name); err != nil {
		t.Fatal(err)
	}
	cf.tick()
	cf.tick()
	if got := cf.servers()[0]; got.State != model.CloudDeleted {
		t.Errorf("a paused cloud node must be drained and deleted: %+v", got)
	}
}

func TestCloudReconcile(t *testing.T) {
	cf := newCloud(t, "alpha")
	fp := cf.fakes["alpha"]
	ctx := context.Background()
	// Ours but untracked, created long ago; and another cluster's.
	fp.servers["90"] = cloud.Server{ID: "90", Name: "cloud-alpha-old", Cluster: cf.cloud.Cluster(), Created: cf.now.Add(-time.Hour)}
	fp.servers["91"] = cloud.Server{ID: "91", Name: "theirs", Cluster: "someone-else", Created: cf.now.Add(-time.Hour)}
	// Ours, just created by another process: left alone for now.
	fp.servers["92"] = cloud.Server{ID: "92", Name: "cloud-alpha-new", Cluster: cf.cloud.Cluster(), Created: cf.now}
	if err := cf.cloud.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cf.cloud.Advance(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cf.cloud.Advance(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := fp.servers["90"]; ok {
		t.Error("an untracked server with our tag must be deleted")
	}
	if _, ok := fp.servers["91"]; !ok {
		t.Error("another cluster's server must never be deleted")
	}
	if _, ok := fp.servers["92"]; !ok {
		t.Error("a server created a moment ago must be left for its creator")
	}
	rows := cf.servers()
	if len(rows) != 1 || rows[0].ID != "90" || rows[0].State != model.CloudDeleted {
		t.Errorf("rows = %+v", rows)
	}

	// A tracked server deleted outside Jellyfinner is recorded so.
	r, _ := cf.cloud.Start(ctx, "")
	delete(fp.servers, r.ID)
	cf.now = cf.now.Add(10 * time.Minute)
	if err := cf.cloud.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for _, row := range cf.servers() {
		if row.ID == r.ID && row.State != model.CloudDeleted {
			t.Errorf("vanished server still tracked: %+v", row)
		}
	}

	// A leftover cloud node with no server goes after two reconciles.
	if _, err := cf.svc.AddNode(NodeSpec{Name: "cloud-alpha-stray"}); err != nil {
		t.Fatal(err)
	}
	cf.cloud.Reconcile(ctx)
	if nodes, _ := cf.store.LoadNodes(); len(nodes) != 1 {
		t.Fatalf("removed on the first sight: %+v", nodes)
	}
	cf.cloud.Reconcile(ctx)
	if nodes, _ := cf.store.LoadNodes(); len(nodes) != 0 {
		t.Errorf("stray cloud node kept: %+v", nodes)
	}
}

func TestCheckCloudProviders(t *testing.T) {
	cf := newCloud(t, "alpha")
	if _, err := cf.cloud.Start(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if err := CheckCloudProviders(cf.svc); err != nil {
		t.Fatal(err)
	}
	cf.cfg.Cloud.Providers = nil
	if err := CheckCloudProviders(cf.svc); err == nil || !strings.Contains(err.Error(), "cloud down --wait") {
		t.Errorf("removing a provider with a live server: %v", err)
	}
}

func TestCloudUsageAndEstimate(t *testing.T) {
	cf := newCloud(t, "alpha", "beta")
	ctx := context.Background()
	cf.cloud.cfg.Max = 10
	cf.fakes["alpha"].prices = []cloud.Price{{Type: "t1", Location: "l1", Hourly: 0.10, Currency: "EUR"}}
	cf.fakes["beta"].prices = []cloud.Price{{Type: "t1", Location: "l1", Hourly: 0.20, Currency: "USD"}}

	est, err := cf.cloud.Estimate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(est.Rows) != 4 || est.Rows[0].Provider != "alpha" || est.Rows[2].Provider != "beta" {
		t.Fatalf("rows must follow the operator's order: %+v", est.Rows)
	}
	if est.Rows[0].Hourly != 0.10 || est.Rows[0].Jobs != 0 || est.Rows[0].QueueCost != 0 {
		t.Errorf("before history, only prices: %+v", est.Rows[0])
	}

	// One server on each, 90 minutes each: two billed hours apiece.
	a, _ := cf.cloud.Start(ctx, "alpha")
	b, _ := cf.cloud.Start(ctx, "beta")
	cf.queue("done1", model.PriorityNew)
	j, _ := cf.store.Claim(a.Name, time.Hour)
	cf.store.Complete(j.ID, j.LeaseID, "{}", "")
	cf.store.MarkDone(j.ID)
	cf.now = cf.now.Add(90 * time.Minute)

	u, err := cf.cloud.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u.HoursToday != 4 || !near(u.SpendToday["EUR"], 0.20) || !near(u.SpendToday["USD"], 0.40) {
		t.Errorf("usage = %+v", u)
	}
	views, _ := cf.cloud.Servers()
	if len(views) != 2 || !near(views[0].Cost, 0.20) || !views[0].PaidUntil.Equal(time.Unix(a.Created, 0).Add(2*time.Hour)) {
		t.Errorf("server views = %+v", views)
	}
	_ = b

	cf.queue("waiting", model.PriorityNew) // 2 GB
	est, err = cf.cloud.Estimate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	row := est.Rows[0]
	// 2 GB done in 2 billed hours: 1 GB per hour, 0.20 EUR per title.
	if row.Jobs != 1 || !near(row.GBPerHour, 1) || !near(row.CostPerTitle, 0.20) || !near(row.QueueHours, 2) || !near(row.QueueCost, 0.20) {
		t.Errorf("measured row = %+v", row)
	}
	if est.Rows[2].Jobs != 0 {
		t.Errorf("beta has no finished work: %+v", est.Rows[2])
	}

	// Prices are cached: a provider whose API fails still has its quote.
	hits := cf.fakes["alpha"].priceHits
	cf.fakes["alpha"].priceErr = errors.New("down")
	if p, _ := cf.cloud.price(ctx, cf.fakes["alpha"], "t1", "l1"); p.Hourly != 0.10 || cf.fakes["alpha"].priceHits != hits {
		t.Errorf("cached price = %+v, hits %d", p, cf.fakes["alpha"].priceHits)
	}
	cf.now = cf.now.Add(7 * time.Hour)
	if p, _ := cf.cloud.price(ctx, cf.fakes["alpha"], "t1", "l1"); p.Hourly != 0.10 {
		t.Errorf("stale price must survive an API failure: %+v", p)
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
