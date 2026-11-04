package queue

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func open(t *testing.T) (*Store, *clock) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := &clock{t: time.Unix(1000000, 0)}
	s.Now = c.now
	s.RetryDelay = 0
	return s, c
}

func addJob(t *testing.T, s *Store, id string, priority int) {
	t.Helper()
	ok, err := s.Enqueue(&Job{ID: id, Path: id + ".mkv", OutputPath: id + ".mp4", Priority: priority})
	if err != nil || !ok {
		t.Fatalf("Enqueue(%s) = %v, %v", id, ok, err)
	}
}

func claim(t *testing.T, s *Store) *Job {
	t.Helper()
	j, err := s.Claim("worker1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestEnqueueDedupes(t *testing.T) {
	s, _ := open(t)
	addJob(t, s, "a", PriorityNew)
	ok, err := s.Enqueue(&Job{ID: "a", Path: "a.mkv", OutputPath: "a.mp4"})
	if err != nil || ok {
		t.Errorf("duplicate Enqueue = %v, %v", ok, err)
	}
}

func TestClaimOrder(t *testing.T) {
	s, c := open(t)
	if err := s.SetMeta("backfill_active", "1"); err != nil {
		t.Fatal(err)
	}
	addJob(t, s, "backfill", PriorityBackfill)
	c.add(time.Second)
	addJob(t, s, "first", PriorityNew)
	c.add(time.Second)
	addJob(t, s, "second", PriorityNew)

	for _, want := range []string{"first", "second", "backfill"} {
		if j := claim(t, s); j == nil || j.ID != want {
			t.Fatalf("claimed %+v, want %s", j, want)
		}
	}
	if j := claim(t, s); j != nil {
		t.Errorf("empty queue gave %+v", j)
	}
}

func TestLeaseExpiryRequeuesThenFails(t *testing.T) {
	s, c := open(t)
	addJob(t, s, "a", PriorityNew)
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
	if j, _ := s.Get("a"); j.State != StateFailed {
		t.Errorf("state = %s", j.State)
	}
	if j := claim(t, s); j != nil {
		t.Errorf("a failed job was claimed: %+v", j)
	}
}

func TestRetryDelayGrowsWithAttempts(t *testing.T) {
	s, c := open(t)
	s.RetryDelay = time.Minute
	addJob(t, s, "a", PriorityNew)

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

func TestHeartbeatExtendsLease(t *testing.T) {
	s, c := open(t)
	addJob(t, s, "a", PriorityNew)
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
	if got.State != StateRunning || got.Stage != "encode" || got.Fraction != 0.5 {
		t.Errorf("job = %+v", got)
	}
}

func TestStaleLeaseCannotAct(t *testing.T) {
	s, c := open(t)
	addJob(t, s, "a", PriorityNew)
	old := claim(t, s)
	c.add(2 * time.Minute)
	if _, err := s.ExpireLeases(3); err != nil {
		t.Fatal(err)
	}
	current := claim(t, s)

	if _, err := s.Heartbeat("a", old.LeaseID, "encode", 0, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("Heartbeat: %v", err)
	}
	if err := s.Complete("a", old.LeaseID, "{}", ""); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("Complete: %v", err)
	}
	if _, err := s.Fail("a", old.LeaseID, "x", "", 3); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("Fail: %v", err)
	}
	if err := s.Complete("a", current.LeaseID, "{}", ""); err != nil {
		t.Errorf("the current lease must work: %v", err)
	}
	if err := s.Complete("a", "", "{}", ""); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("an empty lease must never match: %v", err)
	}
}

func TestFailRetriesThenStops(t *testing.T) {
	s, _ := open(t)
	addJob(t, s, "a", PriorityNew)
	for attempt := 1; attempt <= 2; attempt++ {
		j := claim(t, s)
		final, err := s.Fail(j.ID, j.LeaseID, "boom", "the log", 2)
		if err != nil || final != (attempt == 2) {
			t.Fatalf("attempt %d: %v, %v", attempt, final, err)
		}
	}
	j, _ := s.Get("a")
	if j.State != StateFailed || j.Error != "boom" || j.Log != "the log" {
		t.Errorf("job = %+v", j)
	}
	if err := s.Retry("a"); err != nil {
		t.Fatal(err)
	}
	if j, _ := s.Get("a"); j.State != StateQueued || j.Attempts != 0 {
		t.Errorf("after retry: %+v", j)
	}
}

func TestCancelRunningReachesWorker(t *testing.T) {
	s, _ := open(t)
	addJob(t, s, "a", PriorityNew)
	j := claim(t, s)
	if err := s.Cancel("a"); err != nil {
		t.Fatal(err)
	}
	cancel, err := s.Heartbeat(j.ID, j.LeaseID, "encode", 0.1, time.Minute)
	if err != nil || !cancel {
		t.Errorf("Heartbeat = %v, %v", cancel, err)
	}
	if err := s.Complete(j.ID, j.LeaseID, "{}", ""); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("a cancelled job must not publish: %v", err)
	}
	if _, err := s.Fail(j.ID, j.LeaseID, "stopped", "partial log", 3); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("a"); got.State != StateCancelled || got.Log != "partial log" {
		t.Errorf("job = %+v", got)
	}
	if err := s.Cancel("a"); err == nil {
		t.Error("cancelling twice must report it")
	}
}

func TestCancelForPathKeepsOne(t *testing.T) {
	s, _ := open(t)
	for _, id := range []string{"old", "new"} {
		if _, err := s.Enqueue(&Job{ID: id, Path: "x.mkv", OutputPath: "x.mp4"}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.CancelForPath("x.mkv", "new"); err != nil || n != 1 {
		t.Fatalf("CancelForPath = %d, %v", n, err)
	}
	if j, _ := s.Get("new"); j.State != StateQueued {
		t.Errorf("kept job = %+v", j)
	}
}

func TestPublishFailed(t *testing.T) {
	s, _ := open(t)
	addJob(t, s, "a", PriorityNew)
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
	if got, _ := s.Get("a"); got.State != StateQueued {
		t.Errorf("job = %+v", got)
	}
}

func TestCheckMode(t *testing.T) {
	s, _ := open(t)
	if err := s.CheckMode("auto"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckMode("auto"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckMode("manual"); err == nil || !strings.Contains(err.Error(), "mode switch") {
		t.Errorf("a changed mode must be refused: %v", err)
	}
}

func TestSourceLifecycle(t *testing.T) {
	s, _ := open(t)
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
	if _, err := s.Source("a.mkv"); !errors.Is(err, ErrNotFound) {
		t.Errorf("old path: %v", err)
	}
	if rev, err := s.BumpRevision("b.mkv"); err != nil || rev != 1 {
		t.Errorf("BumpRevision = %d, %v", rev, err)
	}
	if _, err := s.BumpRevision("missing.mkv"); !errors.Is(err, ErrNotFound) {
		t.Errorf("BumpRevision missing: %v", err)
	}
}

func addSized(t *testing.T, s *Store, id string, size int64) {
	t.Helper()
	if _, err := s.Enqueue(&Job{ID: id, Path: id, OutputPath: id, SourceSize: size, Priority: PriorityBackfill}); err != nil {
		t.Fatal(err)
	}
}

func TestBackfillPausedIsNotClaimed(t *testing.T) {
	s, _ := open(t)
	addSized(t, s, "a", 1)
	if j := claim(t, s); j != nil {
		t.Fatalf("a job claimed without backfill started: %+v", j)
	}
	if err := s.SetMeta("backfill_active", "1"); err != nil {
		t.Fatal(err)
	}
	if j := claim(t, s); j == nil {
		t.Fatal("not claimed once active")
	}
	addSized(t, s, "b", 1)
	if err := s.SetMeta("backfill_active", "0"); err != nil {
		t.Fatal(err)
	}
	if j := claim(t, s); j != nil {
		t.Errorf("a queued job was claimed after pause: %+v", j)
	}
}

func TestBackfillBudget(t *testing.T) {
	s, c := open(t)
	s.BackfillBudget = 100
	if err := s.SetMeta("backfill_active", "1"); err != nil {
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

	addJob(t, s, "new", PriorityNew)
	if j := claim(t, s); j == nil || j.ID != "new" {
		t.Errorf("the budget must never hold back new titles: %+v", j)
	}

	c.add(24 * time.Hour)
	if j := claim(t, s); j == nil || j.ID != "c" {
		t.Errorf("a new UTC day must reset the budget: %+v", j)
	}
}

func TestBackfillOversizedTitleRunsOnFreshDay(t *testing.T) {
	s, _ := open(t)
	s.BackfillBudget = 100
	if err := s.SetMeta("backfill_active", "1"); err != nil {
		t.Fatal(err)
	}
	addSized(t, s, "huge", 500)
	if j := claim(t, s); j == nil {
		t.Error("a title over the budget must not block backfill forever")
	}
}

func TestCancelStaleProfile(t *testing.T) {
	s, _ := open(t)
	for id, profile := range map[string]string{"old": "p1", "cur": "p2"} {
		if _, err := s.Enqueue(&Job{ID: id, Path: id, OutputPath: id, Profile: profile}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.CancelStaleProfile("p2"); err != nil || n != 1 {
		t.Fatalf("CancelStaleProfile = %d, %v", n, err)
	}
	if j, _ := s.Get("cur"); j.State != StateQueued {
		t.Errorf("current job = %+v", j)
	}
}

// Several workers claim at once, so no job may go to two of them.
func TestConcurrentClaimsNeverShareAJob(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const jobs = 40
	for i := range jobs {
		id := fmt.Sprintf("job%d", i)
		if _, err := s.Enqueue(&Job{ID: id, Path: id + ".mkv", OutputPath: id + ".mp4", Priority: PriorityNew}); err != nil {
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

func TestMigrateRunsOnceAndInOrder(t *testing.T) {
	s, _ := open(t)
	steps := []string{`CREATE TABLE m1 (x INTEGER)`, `ALTER TABLE m1 ADD COLUMN y INTEGER`}
	if err := migrate(s.db, steps[:1]); err != nil {
		t.Fatal(err)
	}
	// Running a step twice would fail, so a pass proves only the new one ran.
	if err := migrate(s.db, steps); err != nil {
		t.Fatal(err)
	}
	if err := migrate(s.db, steps); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO m1 (x, y) VALUES (1, 2)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(s.db, steps[:1]); err == nil {
		t.Error("an older program must refuse a newer database")
	}
}

func TestSetEncodedNeedsATrackedSource(t *testing.T) {
	s, _ := open(t)
	if err := s.SetEncoded("gone.mkv", 1, 2, "p", "h", 3); err == nil {
		t.Error("recording an encode for no source must fail")
	}
}
