package orchestrator

import (
	"context"
	"math"
	"testing"
	"time"

	"jellyfinner/internal/cloud"
	"jellyfinner/internal/model"
)

var nov10 = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

// runJob queues one job and takes it through node: failures first, then a
// publish lasting took, or a final failure when manifest is "fail".
func (f *fixture) runJob(id, node string, size int64, fails int, took time.Duration, manifest string) {
	f.t.Helper()
	if _, err := f.store.Enqueue(&model.Job{ID: id, Path: id + ".mkv", OutputPath: id + ".mp4", Priority: model.PriorityNew, SourceSize: size}); err != nil {
		f.t.Fatal(err)
	}
	claim := func() *model.Job {
		j, err := f.store.Claim(node, time.Hour)
		if err != nil || j == nil || j.ID != id {
			f.t.Fatalf("claim %s on %s = %+v, %v", id, node, j, err)
		}
		return j
	}
	for range fails {
		j := claim()
		f.advance(time.Minute)
		if _, err := f.store.Fail(j.ID, j.LeaseID, "boom", "", 10); err != nil {
			f.t.Fatal(err)
		}
		f.advance(time.Hour)
	}
	j := claim()
	f.advance(took)
	if manifest == "fail" {
		if _, err := f.store.Fail(j.ID, j.LeaseID, "boom", "", 1); err != nil {
			f.t.Fatal(err)
		}
		return
	}
	if err := f.store.Complete(j.ID, j.LeaseID, manifest, ""); err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.MarkDone(j.ID); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) summary(month time.Time, c *Cloud) Summary {
	f.t.Helper()
	s, err := f.svc.Summary(context.Background(), month, c)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func TestSummaryWorkTrafficAndProjection(t *testing.T) {
	f := newFixture(t)
	f.now = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	f.runJob("october", "worker1", 4e9, 0, time.Hour, `{"files":[{"size":1}]}`)

	f.now = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	f.runJob("encode", "worker1", 2e9, 0, time.Hour, `{"video_copy":false,"files":[{"size":400000000},{"size":100000000}]}`)
	f.runJob("copy", "worker1", 1e9, 1, 30*time.Minute, `{"video_copy":true,"files":[{"size":100000000}]}`)
	f.runJob("broken", "worker2", 1e9, 0, time.Hour, `not json`)
	f.runJob("failed", "worker2", 3e9, 0, time.Hour, "fail")
	f.now = nov10

	s := f.summary(nov10, nil)
	if !s.Current || !s.Next.IsZero() || !s.Prev.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("month links: current %v prev %v next %v", s.Current, s.Prev, s.Next)
	}
	if s.CloudOn || s.CloudSpend != nil || s.Backlog.Cloud != nil {
		t.Errorf("cloud parts shown with no cloud: %+v", s)
	}
	if len(s.Work) != 2 || s.Work[0].Name != "worker1" || s.Work[1].Name != "worker2" {
		t.Fatalf("work rows = %+v", s.Work)
	}
	w1, w2 := s.Work[0], s.Work[1]
	if w1.Published != 2 || w1.Copies != 1 || w1.Encodes() != 1 || w1.Retries != 1 || w1.Failed != 0 ||
		w1.SourceBytes != 3e9 || w1.OutputBytes != 6e8 {
		t.Errorf("worker1 = %+v", w1)
	}
	// Timed over the last attempt only: 1 h and 30 min for 3 GB.
	if w1.TimedJobs != 2 || !near(w1.TimedHours, 1.5) || !near(w1.GBPerHour(), 2) {
		t.Errorf("worker1 speed = %d jobs, %.2f h, %.2f GB/h", w1.TimedJobs, w1.TimedHours, w1.GBPerHour())
	}
	// A manifest that will not parse counts as an encode with no output.
	if w2.Published != 1 || w2.Encodes() != 1 || w2.OutputBytes != 0 || w2.Failed != 1 || w2.SourceBytes != 1e9 {
		t.Errorf("worker2 = %+v", w2)
	}
	if s.Total.Published != 3 || s.Total.Failed != 1 || s.TrafficBytes() != 4e9+6e8 {
		t.Errorf("total = %+v, traffic %d", s.Total, s.TrafficBytes())
	}
	// Nine days of November's thirty have passed.
	if !s.Projected || !near(s.ProjectFactor, 30.0/9) || math.Abs(float64(s.ProjectedTraffic)-46e8*30/9) > 1 {
		t.Errorf("projection = %v ×%.3f, %d bytes", s.Projected, s.ProjectFactor, s.ProjectedTraffic)
	}

	oct := f.summary(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), nil)
	if oct.Current || oct.Projected || !oct.Prev.IsZero() || !oct.Next.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("october links: current %v projected %v prev %v next %v", oct.Current, oct.Projected, oct.Prev, oct.Next)
	}
	if oct.Total.Published != 1 || oct.Total.SourceBytes != 4e9 {
		t.Errorf("october total = %+v", oct.Total)
	}
}

func TestSummaryProjectionWaitsTwoDays(t *testing.T) {
	f := newFixture(t)
	f.now = time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	if s := f.summary(f.now, nil); s.Projected {
		t.Error("projected an empty month")
	}
	f.now = time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)
	f.runJob("a", "worker1", 1e9, 0, time.Hour, `{}`)
	f.now = time.Date(2026, 11, 2, 23, 0, 0, 0, time.UTC)
	if s := f.summary(f.now, nil); s.Projected || s.Total.Published != 1 {
		t.Errorf("projected after under two days: %+v", s)
	}
	f.now = time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	if s := f.summary(f.now, nil); !s.Projected || !near(s.ProjectFactor, 15) {
		t.Errorf("not projected after two days: %v ×%v", s.Projected, s.ProjectFactor)
	}
}

func TestParseMonth(t *testing.T) {
	if m, err := ParseMonth("", nov10.Add(5*time.Hour)); err != nil || !m.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("empty = %v, %v", m, err)
	}
	if m, err := ParseMonth("2026-10", nov10); err != nil || !m.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("2026-10 = %v, %v", m, err)
	}
	for _, bad := range []string{"bad", "2026-13", "2026-12", "11-2026"} {
		if _, err := ParseMonth(bad, nov10); err == nil {
			t.Errorf("ParseMonth(%q) accepted", bad)
		}
	}
}

func (cf *cloudFixture) rented(provider, id, typ string, created, deleted time.Time, price float64, currency string) string {
	cf.t.Helper()
	name := "cloud-" + provider + "-" + id
	r := &model.CloudServer{Provider: provider, ID: id, Name: name, Type: typ, Location: "l1", State: model.CloudDeleted,
		Created: created.Unix(), Deleted: deleted.Unix(), HourlyPrice: price, Currency: currency}
	if err := cf.store.AddCloudServer(r); err != nil {
		cf.t.Fatal(err)
	}
	return name
}

func TestSummaryCloudSpendFollowsTheOperatorsOrder(t *testing.T) {
	cf := newCloud(t, "beta", "alpha")
	// Started before midnight on the 1st: one billed hour each side of it.
	betaNode := cf.rented("beta", "b1", "t1", time.Date(2026, 10, 31, 23, 30, 0, 0, time.UTC),
		time.Date(2026, 11, 1, 1, 10, 0, 0, time.UTC), 0.5, "EUR")
	alphaNode := cf.rented("alpha", "a1", "t2", time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 5, 10, 40, 0, 0, time.UTC), 1, "USD")

	cf.now = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	cf.runJob("by-alpha", alphaNode, 2e9, 0, 30*time.Minute, `{}`)
	cf.now = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	cf.runJob("by-beta", betaNode, 1e9, 0, 30*time.Minute, `{}`)
	cf.runJob("own", "worker1", 1e9, 0, 30*time.Minute, `{}`)
	cf.now = nov10

	s := cf.summary(nov10, cf.cloud)
	if len(s.Work) != 3 || s.Work[0].Name != "worker1" || s.Work[1].Name != "beta t1" || s.Work[2].Name != "alpha t2" {
		t.Fatalf("work order = %+v", s.Work)
	}
	if len(s.CloudSpend) != 2 {
		t.Fatalf("spend = %+v", s.CloudSpend)
	}
	beta, alpha := s.CloudSpend[0], s.CloudSpend[1]
	if beta.Provider != "beta" || beta.Hours != 1 || !near(beta.Cost, 0.5) || beta.Titles != 1 || !near(beta.CostPerTitle(), 0.5) {
		t.Errorf("beta = %+v", beta)
	}
	if alpha.Provider != "alpha" || alpha.Hours != 1 || !near(alpha.Cost, 1) || alpha.Currency != "USD" {
		t.Errorf("alpha = %+v", alpha)
	}
	// Never converted: one total per currency.
	if len(s.CloudTotal) != 2 || s.CloudTotal[0] != (Money{"EUR", 0.5}) || s.CloudTotal[1] != (Money{"USD", 1}) {
		t.Errorf("totals = %+v", s.CloudTotal)
	}
	if len(s.ProjectedCloud) != 2 || !near(s.ProjectedCloud[0].Amount, 0.5*30/9) {
		t.Errorf("projected = %+v", s.ProjectedCloud)
	}

	oct := cf.summary(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), cf.cloud)
	if len(oct.CloudSpend) != 1 || oct.CloudSpend[0].Hours != 1 || oct.CloudSpend[0].Titles != 0 || oct.CloudSpend[0].CostPerTitle() != 0 {
		t.Errorf("october spend = %+v", oct.CloudSpend)
	}

	// Usage, built on the same helper, agrees for today and the month.
	cf.now = time.Date(2026, 11, 5, 23, 0, 0, 0, time.UTC)
	u, err := cf.cloud.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.HoursToday != 1 || u.SpendToday["USD"] != 1 || len(u.Spend) != 2 || u.Spend[0].Provider != "beta" || !near(u.Spend[0].Month, 0.5) {
		t.Errorf("usage = %+v", u)
	}
}

func TestBacklogForecast(t *testing.T) {
	cf := newCloud(t, "alpha")
	cf.fakes["alpha"].prices = []cloud.Price{{Type: "t1", Location: "l1", Hourly: 0.25, Currency: "EUR"}}
	cf.now = nov10

	s := cf.summary(nov10, cf.cloud)
	if s.Backlog.Titles != 0 || s.Backlog.OwnGBPerHour != 0 || s.Backlog.OwnDays() != 0 {
		t.Errorf("empty backlog = %+v", s.Backlog)
	}

	// Two workers measured at 2 and 1 GB an hour run side by side.
	cf.runJob("w1", "worker1", 2e9, 0, time.Hour, `{}`)
	cf.runJob("w2", "worker2", 1e9, 0, time.Hour, `{}`)
	// A rented server's speed is not the operator's own.
	node := cf.rented("alpha", "a1", "t1", cf.now, cf.now.Add(time.Hour), 0.25, "EUR")
	cf.runJob("rented", node, 8e9, 0, time.Hour, `{}`)
	for i := range 3 {
		cf.queue(string(rune('a'+i)), model.PriorityNew)
	}

	s = cf.summary(nov10, cf.cloud)
	b := s.Backlog
	if b.Titles != 3 || b.Bytes != 6e9 {
		t.Fatalf("backlog = %d titles, %d bytes", b.Titles, b.Bytes)
	}
	if b.OwnWorkers != 2 || b.OwnJobs != 2 || !near(b.OwnGBPerHour, 3) || !near(b.OwnHours, 2) || b.BudgetBound() {
		t.Errorf("own forecast = %+v", b)
	}
	// Cloud rows: t1 measured at 8 GB/h, t2 not yet.
	if len(b.Cloud) != 2 || b.Cloud[0].Type != "t1" || !near(b.Cloud[0].BacklogHours, 0.75) || !near(b.Cloud[0].BacklogCost, 0.25) {
		t.Errorf("cloud backlog = %+v", b.Cloud)
	}
	if b.Cloud[1].Jobs != 0 || b.Cloud[1].BacklogHours != 0 {
		t.Errorf("unmeasured type forecast: %+v", b.Cloud[1])
	}
}

func TestBacklogBoundByTheBackfillBudget(t *testing.T) {
	f := newFixture(t)
	f.library3(3) // three backlog titles of 6 bytes each
	f.cfg.BackfillDailyBytes = 4
	f.now = nov10
	f.runJob("w1", "worker1", 1e9, 0, time.Hour, `{}`)

	b := f.summary(nov10, nil).Backlog
	if b.Titles != 3 || b.BackfillBytes != 18 || !near(b.BudgetDays, 4.5) || !b.BudgetBound() || !near(b.OwnDays(), 4.5) {
		t.Errorf("budget-bound backlog = %+v", b)
	}
}
