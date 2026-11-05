// Package dbtest is the behaviour every database.Store backend must have. A
// backend's own test calls Run; other packages' tests use Open for a real,
// throwaway store.
package dbtest

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyfinner/internal/database"
	"jellyfinner/internal/database/sqlite"
	"jellyfinner/internal/model"
)

// Opener opens an empty store for one test.
type Opener func(t *testing.T, opts database.Options) database.Store

// Run checks a backend against every behaviour the rest of the program relies on.
func Run(t *testing.T, open Opener) {
	x := suite{opener: open}
	v := reflect.ValueOf(x)
	for i := range v.NumMethod() {
		name := v.Type().Method(i).Name
		if !strings.HasPrefix(name, "Test") {
			continue
		}
		method := v.Method(i)
		t.Run(strings.TrimPrefix(name, "Test"), func(t *testing.T) {
			method.Call([]reflect.Value{reflect.ValueOf(t)})
		})
	}
}

// Open returns a SQLite store in a temporary directory, closed when the test ends.
func Open(t *testing.T, opts database.Options) database.Store {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

type suite struct{ opener Opener }

// open starts a store on a test clock, with no retry delay unless opts says.
func (x suite) open(t *testing.T, opts database.Options) (database.Store, *clock) {
	t.Helper()
	c := &clock{t: time.Unix(1000000, 0)}
	opts.Now = c.now
	s := x.opener(t, opts)
	t.Cleanup(func() { s.Close() })
	return s, c
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func addJob(t *testing.T, s database.Store, id string, priority int) {
	t.Helper()
	ok, err := s.Enqueue(&model.Job{ID: id, Path: id + ".mkv", OutputPath: id + ".mp4", Priority: priority})
	if err != nil || !ok {
		t.Fatalf("Enqueue(%s) = %v, %v", id, ok, err)
	}
}

func claim(t *testing.T, s database.Store) *model.Job {
	t.Helper()
	j, err := s.Claim("worker1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func (x suite) TestEnqueueDedupes(t *testing.T) {
	s, _ := x.open(t, database.Options{})
	addJob(t, s, "a", model.PriorityNew)
	ok, err := s.Enqueue(&model.Job{ID: "a", Path: "a.mkv", OutputPath: "a.mp4"})
	if err != nil || ok {
		t.Errorf("duplicate Enqueue = %v, %v", ok, err)
	}
}

func (x suite) TestClaimOrder(t *testing.T) {
	s, c := x.open(t, database.Options{})
	if err := s.SetMeta(model.MetaBackfillActive, "1"); err != nil {
		t.Fatal(err)
	}
	addJob(t, s, "backfill", model.PriorityBackfill)
	c.add(time.Second)
	addJob(t, s, "first", model.PriorityNew)
	c.add(time.Second)
	addJob(t, s, "second", model.PriorityNew)

	for _, want := range []string{"first", "second", "backfill"} {
		if j := claim(t, s); j == nil || j.ID != want {
			t.Fatalf("claimed %+v, want %s", j, want)
		}
	}
	if j := claim(t, s); j != nil {
		t.Errorf("empty queue gave %+v", j)
	}
}

func (x suite) TestLeaseExpiryRequeuesThenFails(t *testing.T) {
	s, c := x.open(t, database.Options{})
	addJob(t, s, "a", model.PriorityNew)
	for attempt := 1; attempt <= 3; attempt++ {
		j := claim(t, s)
		if j == nil || j.Attempts != attempt {
			t.Fatalf("attempt %d: %+v", attempt, j)
		}
		c.add(30 * time.Second)
		if failed, err := s.ExpireLeases(3); err != nil || len(failed) != 0 {
			t.Fatalf("a live lease expired: %v, %v", failed, err)
		}
		c.add(time.Minute)
		failed, err := s.ExpireLeases(3)
		if err != nil {
			t.Fatal(err)
		}
		if (len(failed) == 1) != (attempt == 3) {
			t.Fatalf("attempt %d: failed = %v", attempt, failed)
		}
	}
	if j, _ := s.Get("a"); j.State != model.StateFailed {
		t.Errorf("state = %s", j.State)
	}
	if j := claim(t, s); j != nil {
		t.Errorf("a failed job was claimed: %+v", j)
	}
}

func (x suite) TestRetryDelayGrowsWithAttempts(t *testing.T) {
	s, c := x.open(t, database.Options{RetryDelay: time.Minute})
	addJob(t, s, "a", model.PriorityNew)

	j := claim(t, s)
	if _, err := s.Fail(j.ID, j.LeaseID, "sftp down", "", 3); err != nil {
		t.Fatal(err)
	}
	if got := claim(t, s); got != nil {
		t.Fatal("a failed attempt must wait before retrying")
	}
	c.add(time.Minute)
	j = claim(t, s)
	if j == nil {
		t.Fatal("not retried after the delay")
	}
	if _, err := s.Fail(j.ID, j.LeaseID, "sftp down", "", 3); err != nil {
		t.Fatal(err)
	}
	c.add(time.Minute)
	if got := claim(t, s); got != nil {
		t.Fatal("the second delay must be longer")
	}
	c.add(time.Minute)
	if j = claim(t, s); j == nil {
		t.Fatal("not retried after the second delay")
	}
	if _, err := s.Fail(j.ID, j.LeaseID, "sftp down", "", 3); err != nil {
		t.Fatal(err)
	}
	if err := s.Retry("a"); err != nil {
		t.Fatal(err)
	}
	if got := claim(t, s); got == nil {
		t.Error("an operator's retry must run straight away")
	}
}

func (x suite) TestHeartbeatExtendsLease(t *testing.T) {
	s, c := x.open(t, database.Options{})
	addJob(t, s, "a", model.PriorityNew)
	j := claim(t, s)
	c.add(50 * time.Second)
	if cancel, err := s.Heartbeat(j.ID, j.LeaseID, "encode", 0.5, time.Minute); err != nil || cancel {
		t.Fatalf("Heartbeat = %v, %v", cancel, err)
	}
	c.add(50 * time.Second)
	if failed, err := s.ExpireLeases(3); err != nil || len(failed) != 0 {
		t.Fatal(failed, err)
	}
	got, _ := s.Get("a")
	if got.State != model.StateRunning || got.Stage != "encode" || got.Fraction != 0.5 {
		t.Errorf("job = %+v", got)
	}
}

func (x suite) TestStaleLeaseCannotAct(t *testing.T) {
	s, c := x.open(t, database.Options{})
	addJob(t, s, "a", model.PriorityNew)
	old := claim(t, s)
	c.add(2 * time.Minute)
	if _, err := s.ExpireLeases(3); err != nil {
		t.Fatal(err)
	}
	current := claim(t, s)

	if _, err := s.Heartbeat("a", old.LeaseID, "encode", 0, time.Minute); !errors.Is(err, model.ErrLeaseLost) {
		t.Errorf("Heartbeat: %v", err)
	}
	if err := s.Complete("a", old.LeaseID, "{}", ""); !errors.Is(err, model.ErrLeaseLost) {
		t.Errorf("Complete: %v", err)
	}
	if _, err := s.Fail("a", old.LeaseID, "x", "", 3); !errors.Is(err, model.ErrLeaseLost) {
		t.Errorf("Fail: %v", err)
	}
	if err := s.Complete("a", current.LeaseID, "{}", ""); err != nil {
		t.Errorf("the current lease must work: %v", err)
	}
	if err := s.Complete("a", "", "{}", ""); !errors.Is(err, model.ErrLeaseLost) {
		t.Errorf("an empty lease must never match: %v", err)
	}
}

func (x suite) TestFailRetriesThenStops(t *testing.T) {
	s, _ := x.open(t, database.Options{})
	addJob(t, s, "a", model.PriorityNew)
	for attempt := 1; attempt <= 2; attempt++ {
		j := claim(t, s)
		final, err := s.Fail(j.ID, j.LeaseID, "boom", "the log", 2)
		if err != nil || final != (attempt == 2) {
			t.Fatalf("attempt %d: %v, %v", attempt, final, err)
		}
	}
	j, _ := s.Get("a")
	if j.State != model.StateFailed || j.Error != "boom" || j.Log != "the log" {
		t.Errorf("job = %+v", j)
	}
	if err := s.Retry("a"); err != nil {
		t.Fatal(err)
	}
	if j, _ := s.Get("a"); j.State != model.StateQueued || j.Attempts != 0 {
		t.Errorf("after retry: %+v", j)
	}
}

func (x suite) TestFailedByNodeCountsRecentFinalFailures(t *testing.T) {
	s, c := x.open(t, database.Options{})
	addJob(t, s, "old", model.PriorityNew)
	j := claim(t, s)
	if _, err := s.Fail(j.ID, j.LeaseID, "boom", "", 1); err != nil {
		t.Fatal(err)
	}
	c.add(25 * time.Hour)
	addJob(t, s, "new", model.PriorityNew)
	j = claim(t, s)
	if _, err := s.Fail(j.ID, j.LeaseID, "boom", "", 1); err != nil {
		t.Fatal(err)
	}
	addJob(t, s, "retrying", model.PriorityNew)
	j = claim(t, s)
	if _, err := s.Fail(j.ID, j.LeaseID, "boom", "", 3); err != nil {
		t.Fatal(err)
	}
	got, err := s.FailedByNode(c.now().Add(-24 * time.Hour))
	if err != nil || len(got) != 1 || got["worker1"] != 1 {
		t.Errorf("FailedByNode = %v, %v", got, err)
	}
}

func (x suite) TestListFiltered(t *testing.T) {
	s, _ := x.open(t, database.Options{})
	for _, id := range []string{"a", "b", "c", "100%_x"} {
		addJob(t, s, id, model.PriorityNew)
	}
	j := claim(t, s)
	if _, err := s.Fail(j.ID, j.LeaseID, "boom", "", 1); err != nil {
		t.Fatal(err)
	}
	ids := func(f model.JobFilter, limit, offset int) (string, int) {
		t.Helper()
		jobs, total, err := s.ListFiltered(f, limit, offset)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, j := range jobs {
			got = append(got, j.ID)
		}
		return strings.Join(got, ","), total
	}
	if got, total := ids(model.JobFilter{State: model.StateFailed, Node: "worker1"}, 10, 0); got != j.ID || total != 1 {
		t.Errorf("failed on worker1 = %q, %d", got, total)
	}
	if got, total := ids(model.JobFilter{Node: "worker2"}, 10, 0); got != "" || total != 0 {
		t.Errorf("worker2 = %q, %d", got, total)
	}
	if _, total := ids(model.JobFilter{State: model.StateQueued}, 10, 0); total != 3 {
		t.Errorf("queued total = %d", total)
	}
	first, total := ids(model.JobFilter{}, 2, 0)
	second, _ := ids(model.JobFilter{}, 2, 2)
	if total != 4 || len(strings.Split(first, ",")) != 2 || len(strings.Split(second, ",")) != 2 || first == second {
		t.Errorf("pages = %q then %q of %d", first, second, total)
	}
	// % and _ in a search are literal, and case does not matter.
	if got, _ := ids(model.JobFilter{Search: "0%_X"}, 10, 0); got != "100%_x" {
		t.Errorf("literal search = %q", got)
	}
	if got, _ := ids(model.JobFilter{Search: "%"}, 10, 0); got != "100%_x" {
		t.Errorf("percent search = %q", got)
	}
}

func (x suite) TestCancelRunningReachesWorker(t *testing.T) {
	s, _ := x.open(t, database.Options{})
	addJob(t, s, "a", model.PriorityNew)
	j := claim(t, s)
	if err := s.Cancel("a"); err != nil {
		t.Fatal(err)
	}
	cancel, err := s.Heartbeat(j.ID, j.LeaseID, "encode", 0.1, time.Minute)
	if err != nil || !cancel {
		t.Errorf("Heartbeat = %v, %v", cancel, err)
	}
	if err := s.Complete(j.ID, j.LeaseID, "{}", ""); !errors.Is(err, model.ErrLeaseLost) {
		t.Errorf("a cancelled job must not publish: %v", err)
	}
	if _, err := s.Fail(j.ID, j.LeaseID, "stopped", "partial log", 3); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("a"); got.State != model.StateCancelled || got.Log != "partial log" {
		t.Errorf("job = %+v", got)
	}
	if err := s.Cancel("a"); err == nil {
		t.Error("cancelling twice must report it")
	}
}

func (x suite) TestCancelForPathKeepsOne(t *testing.T) {
	s, _ := x.open(t, database.Options{})
	for _, id := range []string{"old", "new"} {
		if _, err := s.Enqueue(&model.Job{ID: id, Path: "x.mkv", OutputPath: "x.mp4"}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.CancelForPath("x.mkv", "new"); err != nil || n != 1 {
		t.Fatalf("CancelForPath = %d, %v", n, err)
	}
	if j, _ := s.Get("new"); j.State != model.StateQueued {
		t.Errorf("kept job = %+v", j)
	}
}

func (x suite) TestPublishFailed(t *testing.T) {
	s, _ := x.open(t, database.Options{})
	addJob(t, s, "a", model.PriorityNew)
	j := claim(t, s)
	if err := s.Complete(j.ID, j.LeaseID, "{}", ""); err != nil {
		t.Fatal(err)
	}
	if list, err := s.Publishing(); err != nil || len(list) != 1 {
		t.Fatalf("Publishing = %v, %v", list, err)
	}
	final, err := s.PublishFailed("a", "bad checksum", 3)
	if err != nil || final {
		t.Fatalf("PublishFailed = %v, %v", final, err)
	}
	if got, _ := s.Get("a"); got.State != model.StateQueued {
		t.Errorf("job = %+v", got)
	}
}

func (x suite) TestSourceLifecycle(t *testing.T) {
	s, _ := x.open(t, database.Options{})
	if err := s.AddSource("a.mkv", 10, 20, true); err != nil {
		t.Fatal(err)
	}
	src, err := s.Source("a.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if src.Encoded() || !src.Backlog {
		t.Errorf("new source = %+v", src)
	}
	if err := s.SetEncoded("a.mkv", 10, 20, "p", "h", 5); err != nil {
		t.Fatal(err)
	}
	if src, _ = s.Source("a.mkv"); !src.Encoded() {
		t.Errorf("after publish = %+v", src)
	}
	if err := s.UpdateSource("a.mkv", 11, 21); err != nil {
		t.Fatal(err)
	}
	if src, _ = s.Source("a.mkv"); src.Encoded() {
		t.Errorf("a replaced file is not encoded: %+v", src)
	}
	if err := s.RenameSource("a.mkv", "b.mkv"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Source("a.mkv"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("old path: %v", err)
	}
	if rev, err := s.BumpRevision("b.mkv"); err != nil || rev != 1 {
		t.Errorf("BumpRevision = %d, %v", rev, err)
	}
	if _, err := s.BumpRevision("missing.mkv"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("BumpRevision missing: %v", err)
	}
}

func addSized(t *testing.T, s database.Store, id string, size int64) {
	t.Helper()
	if _, err := s.Enqueue(&model.Job{ID: id, Path: id, OutputPath: id, SourceSize: size, Priority: model.PriorityBackfill}); err != nil {
		t.Fatal(err)
	}
}

func (x suite) TestBackfillPausedIsNotClaimed(t *testing.T) {
	s, _ := x.open(t, database.Options{})
	addSized(t, s, "a", 1)
	if j := claim(t, s); j != nil {
		t.Fatalf("a job claimed without backfill started: %+v", j)
	}
	if err := s.SetMeta(model.MetaBackfillActive, "1"); err != nil {
		t.Fatal(err)
	}
	if j := claim(t, s); j == nil {
		t.Fatal("not claimed once active")
	}
	addSized(t, s, "b", 1)
	if err := s.SetMeta(model.MetaBackfillActive, "0"); err != nil {
		t.Fatal(err)
	}
	if j := claim(t, s); j != nil {
		t.Errorf("a queued job was claimed after pause: %+v", j)
	}
}

func (x suite) TestBackfillBudget(t *testing.T) {
	s, c := x.open(t, database.Options{BackfillBudget: 100})
	if err := s.SetMeta(model.MetaBackfillActive, "1"); err != nil {
		t.Fatal(err)
	}
	addSized(t, s, "a", 60)
	c.add(time.Second)
	addSized(t, s, "b", 30)
	c.add(time.Second)
	addSized(t, s, "c", 30)

	if j := claim(t, s); j == nil || j.ID != "a" {
		t.Fatalf("claim = %+v", j)
	}
	if j := claim(t, s); j == nil || j.ID != "b" {
		t.Fatalf("claim = %+v", j)
	}
	if j := claim(t, s); j != nil {
		t.Fatalf("over budget: %+v", j)
	}
	if used, err := s.BudgetUsedToday(); err != nil || used != 90 {
		t.Errorf("used = %d, %v", used, err)
	}

	addJob(t, s, "new", model.PriorityNew)
	if j := claim(t, s); j == nil || j.ID != "new" {
		t.Errorf("the budget must never hold back new titles: %+v", j)
	}

	c.add(24 * time.Hour)
	if j := claim(t, s); j == nil || j.ID != "c" {
		t.Errorf("a new UTC day must reset the budget: %+v", j)
	}
}

func (x suite) TestBackfillOversizedTitleRunsOnFreshDay(t *testing.T) {
	s, _ := x.open(t, database.Options{BackfillBudget: 100})
	if err := s.SetMeta(model.MetaBackfillActive, "1"); err != nil {
		t.Fatal(err)
	}
	addSized(t, s, "huge", 500)
	if j := claim(t, s); j == nil {
		t.Error("a title over the budget must not block backfill forever")
	}
}

func (x suite) TestCancelStaleProfile(t *testing.T) {
	s, _ := x.open(t, database.Options{})
	for id, profile := range map[string]string{"old": "p1", "cur": "p2"} {
		if _, err := s.Enqueue(&model.Job{ID: id, Path: id, OutputPath: id, Profile: profile}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.CancelStaleProfile("p2"); err != nil || n != 1 {
		t.Fatalf("CancelStaleProfile = %d, %v", n, err)
	}
	if j, _ := s.Get("cur"); j.State != model.StateQueued {
		t.Errorf("current job = %+v", j)
	}
}

// Several workers claim at once, so no job may go to two of them.
func (x suite) TestConcurrentClaimsNeverShareAJob(t *testing.T) {
	s, _ := x.open(t, database.Options{})
	const jobs = 40
	for i := range jobs {
		id := fmt.Sprintf("job%d", i)
		if _, err := s.Enqueue(&model.Job{ID: id, Path: id + ".mkv", OutputPath: id + ".mp4", Priority: model.PriorityNew}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	claimed := map[string]string{}
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			node := fmt.Sprintf("worker%d", w)
			for {
				j, err := s.Claim(node, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if j == nil {
					return
				}
				mu.Lock()
				if other, dup := claimed[j.ID]; dup {
					t.Errorf("%s went to both %s and %s", j.ID, other, node)
				}
				claimed[j.ID] = node
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != jobs {
		t.Errorf("claimed %d of %d jobs", len(claimed), jobs)
	}
}

func (x suite) TestSetEncodedNeedsATrackedSource(t *testing.T) {
	s, _ := x.open(t, database.Options{})
	if err := s.SetEncoded("gone.mkv", 1, 2, "p", "h", 3); err == nil {
		t.Error("recording an encode for no source must fail")
	}
}

func (x suite) TestPausedNodeClaimsNothing(t *testing.T) {
	s, _ := x.open(t, database.Options{})
	for _, name := range []string{"worker1", "worker2"} {
		if err := s.AddNode(&model.Node{Name: name, Kind: model.NodeWorker, TokenHash: name}); err != nil {
			t.Fatal(err)
		}
	}
	addJob(t, s, "a", model.PriorityNew)
	addJob(t, s, "b", model.PriorityNew)
	running := claim(t, s)
	if running == nil {
		t.Fatal("want a job before the pause")
	}

	if err := s.SetNodePaused("worker1", true); err != nil {
		t.Fatal(err)
	}
	if j := claim(t, s); j != nil {
		t.Fatalf("a paused node claimed %s", j.ID)
	}
	// Another worker still gets the job the paused one left.
	if j, err := s.Claim("worker2", time.Minute); err != nil || j == nil || j.ID != "b" {
		t.Fatalf("worker2 claimed %+v, %v", j, err)
	}
	// The job held before the pause still finishes.
	if err := s.Complete(running.ID, running.LeaseID, "{}", ""); err != nil {
		t.Errorf("a job held before the pause must finish: %v", err)
	}

	nodes, err := s.LoadNodes()
	if err != nil || len(nodes) != 2 || !nodes[0].Paused || nodes[1].Paused {
		t.Fatalf("LoadNodes = %+v, %v", nodes, err)
	}
	if err := s.SetNodePaused("worker1", false); err != nil {
		t.Fatal(err)
	}
	addJob(t, s, "c", model.PriorityNew)
	if j := claim(t, s); j == nil || j.ID != "c" {
		t.Errorf("a resumed node claimed %+v", j)
	}
	if err := s.SetNodePaused("nobody", true); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("pausing an unknown node: %v", err)
	}
}

func (x suite) TestCloudServersKeepHistory(t *testing.T) {
	s, c := x.open(t, database.Options{})
	a := &model.CloudServer{Provider: "p1", ID: "1", Name: "cloud-p1-a", Type: "t", Location: "l",
		State: model.CloudBooting, Created: c.t.Unix(), HourlyPrice: 0.05, Currency: "EUR"}
	b := &model.CloudServer{Provider: "p2", ID: "1", Name: "cloud-p2-b", Type: "t", Location: "l",
		State: model.CloudReady, Created: c.t.Unix() + 1}
	for _, srv := range []*model.CloudServer{a, b} {
		if err := s.AddCloudServer(srv); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddCloudServer(a); err == nil {
		t.Error("the same provider and ID twice must fail")
	}

	c.add(time.Hour)
	a.State, a.Deleted, a.Alerted, a.Resources = model.CloudDeleted, c.t.Unix(), true, "ip:9"
	if err := s.UpdateCloudServer(a); err != nil {
		t.Fatal(err)
	}
	got, err := s.CloudServers(c.t)
	if err != nil || len(got) != 2 {
		t.Fatalf("CloudServers = %+v, %v", got, err)
	}
	if *got[0] != *a {
		t.Errorf("round trip = %+v, want %+v", *got[0], *a)
	}
	// Deleted before since: only the live one remains.
	if got, _ := s.CloudServers(c.t.Add(time.Second)); len(got) != 1 || got[0].Name != b.Name {
		t.Errorf("history cut-off = %+v", got)
	}
	missing := *b
	missing.ID = "nope"
	if err := s.UpdateCloudServer(&missing); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("updating an unknown server: %v", err)
	}
}

func (x suite) TestQueueQueriesForTheScaler(t *testing.T) {
	s, c := x.open(t, database.Options{})
	if ok, err := s.Enqueue(&model.Job{ID: "new", Path: "new.mkv", OutputPath: "new.mp4", Priority: model.PriorityNew, SourceSize: 100}); err != nil || !ok {
		t.Fatal(err)
	}
	c.add(time.Second)
	ok, err := s.Enqueue(&model.Job{ID: "bf", Path: "bf.mkv", OutputPath: "bf.mp4", Priority: model.PriorityBackfill, SourceSize: 50})
	if err != nil || !ok {
		t.Fatal(err)
	}
	w, err := s.Waiting(false)
	if err != nil || w.Count != 1 || w.SourceBytes != 100 || w.Oldest != c.t.Unix()-1 {
		t.Fatalf("Waiting(false) = %+v, %v", w, err)
	}
	// Backfill counts only while it runs.
	if w, _ := s.Waiting(true); w.Count != 1 {
		t.Errorf("paused backfill counted: %+v", w)
	}
	if err := s.SetMeta(model.MetaBackfillActive, "1"); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.Waiting(true); w.Count != 2 || w.SourceBytes != 150 {
		t.Errorf("Waiting(true) = %+v", w)
	}

	j := claim(t, s)
	running, err := s.RunningByNode()
	if err != nil || running["worker1"] != 1 {
		t.Fatalf("RunningByNode = %v, %v", running, err)
	}
	if err := s.Complete(j.ID, j.LeaseID, "{}", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDone(j.ID); err != nil {
		t.Fatal(err)
	}
	work, err := s.WorkByNode(c.t.Add(-time.Hour))
	if err != nil || work["worker1"] != (model.NodeWork{Jobs: 1, SourceBytes: 100}) {
		t.Errorf("WorkByNode = %v, %v", work, err)
	}
	if work, _ := s.WorkByNode(c.t.Add(time.Hour)); len(work) != 0 {
		t.Errorf("work before since counted: %v", work)
	}
}

func (x suite) TestOutcomesAndJobTiming(t *testing.T) {
	s, c := x.open(t, database.Options{})
	if first, err := s.FirstActivity(); err != nil || first != 0 {
		t.Fatalf("FirstActivity on empty store = %d, %v", first, err)
	}
	start := c.t
	for _, id := range []string{"done", "failed", "cancelled", "queued"} {
		ok, err := s.Enqueue(&model.Job{ID: id, Path: id + ".mkv", OutputPath: id + ".mp4", Priority: model.PriorityNew, SourceSize: 10})
		if err != nil || !ok {
			t.Fatal(err)
		}
	}
	if first, _ := s.FirstActivity(); first != start.Unix() {
		t.Errorf("FirstActivity = %d, want %d", first, start.Unix())
	}

	// A failed first attempt, then a retry: the times cover the last attempt.
	j := claim(t, s)
	if j.ID != "done" || j.Started != c.t.Unix() || j.Finished != 0 {
		t.Fatalf("claimed %+v", j)
	}
	c.add(time.Minute)
	if _, err := s.Fail(j.ID, j.LeaseID, "boom", "", 5); err != nil {
		t.Fatal(err)
	}
	c.add(time.Hour)
	j = claim(t, s)
	if j.ID != "done" || j.Attempts != 2 {
		t.Fatalf("retry claimed %+v", j)
	}
	claimedAt := c.t.Unix()
	c.add(30 * time.Minute)
	if err := s.Complete(j.ID, j.LeaseID, `{"video_copy":true}`, ""); err != nil {
		t.Fatal(err)
	}
	finishedAt := c.t.Unix()
	c.add(time.Minute)
	if err := s.MarkDone(j.ID); err != nil {
		t.Fatal(err)
	}

	c.add(time.Minute)
	f := claim(t, s)
	if _, err := s.Fail(f.ID, f.LeaseID, "boom", "", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel("cancelled"); err != nil {
		t.Fatal(err)
	}

	got, err := s.Outcomes(start, c.t.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]model.JobOutcome{}
	for _, o := range got {
		states[o.State] = o
	}
	if len(got) != 3 || len(states) != 3 {
		t.Fatalf("Outcomes = %+v, want done, failed and cancelled only", got)
	}
	d := states[model.StateDone]
	if d.Node != "worker1" || d.Attempts != 2 || d.SourceSize != 10 || d.Started != claimedAt || d.Finished != finishedAt ||
		d.Manifest != `{"video_copy":true}` || !d.Timed() {
		t.Errorf("done outcome = %+v", d)
	}
	if states[model.StateCancelled].Timed() {
		t.Errorf("never-claimed job counts as timed: %+v", states[model.StateCancelled])
	}
	// [from, to): a job ending exactly at to belongs to the next period.
	if got, _ := s.Outcomes(start, c.t); len(got) != 1 || got[0].State != model.StateDone {
		t.Errorf("Outcomes up to now, exclusive = %+v, want only the earlier done job", got)
	}
	if got, _ := s.Outcomes(c.t.Add(time.Second), c.t.Add(time.Hour)); len(got) != 0 {
		t.Errorf("Outcomes after the fact = %+v", got)
	}
}
