package orchestrator

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jellyfinner/internal/encode"
	"jellyfinner/internal/queue"
)

// library writes n existing titles, oldest first, and records them as backlog.
func (f *fixture) library3(n int) []string {
	f.t.Helper()
	var names []string
	for i := range n {
		name := fmt.Sprintf("t%d.mkv", i)
		f.write(name, name)
		stamp := time.Unix(int64(1000+i*1000), 0)
		if err := os.Chtimes(filepath.Join(f.cfg.OriginalsDir, name), stamp, stamp); err != nil {
			f.t.Fatal(err)
		}
		names = append(names, name)
	}
	f.scan()
	return names
}

func (f *fixture) finishClaimed() {
	f.t.Helper()
	j := f.claim()
	f.upload(j, map[string]string{filepath.Base(j.OutputPath): "video"})
	f.complete(j)
	f.svc.PublishPending(context.Background())
}

func TestBackfillDoesNothingUntilStarted(t *testing.T) {
	f := newFixture(t)
	f.library3(3)
	f.advance(time.Hour)
	f.scan()
	if jobs := f.jobs(); len(jobs) != 0 {
		t.Errorf("jobs = %+v", jobs)
	}
}

func TestBackfillFeedsNewestFirstInAWindow(t *testing.T) {
	f := newFixture(t)
	f.library3(6)
	if err := f.svc.SetBackfill(true); err != nil {
		t.Fatal(err)
	}
	f.scan()
	jobs := f.jobs()
	if len(jobs) != backfillWindow {
		t.Fatalf("queued %d, want %d", len(jobs), backfillWindow)
	}
	for _, name := range []string{"t5.mkv", "t4.mkv", "t3.mkv"} {
		j := f.queued(name)
		if j == nil || j.Priority != queue.PriorityBackfill {
			t.Errorf("%s: %+v", name, j)
		}
	}
	f.scan()
	if n := len(f.jobs()); n != backfillWindow {
		t.Errorf("a second scan overfilled the window: %d", n)
	}
}

func TestBackfillFinishesAndNotifiesOnce(t *testing.T) {
	f := newFixture(t)
	f.library3(4)
	if err := f.svc.SetBackfill(true); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		f.scan()
		f.finishClaimed()
	}
	st, err := f.svc.BackfillStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.Remaining != 0 || st.InProgress != 0 {
		t.Errorf("status = %+v", st)
	}
	f.scan()
	f.scan()
	count := 0
	for _, m := range f.notify.messages {
		if strings.Contains(m, "Backfill finished") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("finished messages = %d", count)
	}
}

func TestBackfillFailedTitleDoesNotBlockTheRest(t *testing.T) {
	f := newFixture(t)
	f.library3(2)
	if err := f.svc.SetBackfill(true); err != nil {
		t.Fatal(err)
	}
	f.scan()
	for range f.cfg.MaxAttempts {
		j := f.claim()
		if j.Path != "t1.mkv" {
			t.Fatalf("claimed %s", j.Path)
		}
		if _, err := f.store.Fail(j.ID, j.LeaseID, "boom", "", f.cfg.MaxAttempts); err != nil {
			t.Fatal(err)
		}
	}
	f.scan()
	f.finishClaimed()
	if _, ok := f.inLibrary("t0.mp4"); !ok {
		t.Error("the next title must still run")
	}
	f.scan()
	if !f.notify.has("failed titles") {
		t.Errorf("messages = %v", f.notify.messages)
	}
}

func TestBackfillRefusedInManual(t *testing.T) {
	f := newManual(t)
	if err := f.svc.SetBackfill(true); err == nil {
		t.Error("backfill must refuse manual mode")
	}
}

func TestProfileChangeRebuildsThroughBackfill(t *testing.T) {
	f := newFixture(t)
	f.settled()
	f.write("a.mkv", "a")
	f.finishTitle("a.mkv", "v1")
	f.advance(time.Hour)
	f.scan()
	if f.queued("a.mkv") != nil {
		t.Fatal("nothing changed yet")
	}

	f.cfg.Output = encode.Profile{MaxHeight: 720}
	restarted := New(f.cfg, f.store, f.notify, f.library, log.New(io.Discard, "", 0))
	f.svc = restarted
	f.scan()
	if f.queued("a.mkv") != nil {
		t.Error("a profile change must wait for backfill to be started")
	}
	if err := f.svc.SetBackfill(true); err != nil {
		t.Fatal(err)
	}
	f.scan()
	j := f.queued("a.mkv")
	if j == nil || j.Priority != queue.PriorityBackfill {
		t.Fatalf("job = %+v", j)
	}
	if got := f.svc.WorkerJob(j); got.Profile.MaxHeight != 720 {
		t.Errorf("worker job = %+v", got)
	}
	if got, _ := f.inLibrary("a.mp4"); got != "v1" {
		t.Error("the live encode stays until the rebuild publishes")
	}
}

func TestSwitchManualToAutoThenBackfillEncodesStubs(t *testing.T) {
	f := newManual(t)
	f.write("a.mkv", "a")
	f.stabilise()
	if _, err := f.store.SwitchMode("auto"); err != nil {
		t.Fatal(err)
	}
	f.cfg.Mode = "auto"
	if err := f.svc.SetBackfill(true); err != nil {
		t.Fatal(err)
	}
	f.scan()
	f.finishClaimed()
	if f.isStub("a.mp4") {
		t.Error("backfill must replace the stub")
	}
}

func TestSwitchToManualStopsBackfill(t *testing.T) {
	f := newFixture(t)
	f.library3(1)
	if err := f.svc.SetBackfill(true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SwitchMode("manual"); err != nil {
		t.Fatal(err)
	}
	if st, err := f.svc.BackfillStatus(); err != nil || st.Active {
		t.Errorf("status = %+v, %v", st, err)
	}
}

func TestMetrics(t *testing.T) {
	f := newFixture(t)
	f.library3(2)
	if err := f.svc.SetBackfill(true); err != nil {
		t.Fatal(err)
	}
	f.scan()
	w := httptest.NewRecorder()
	f.svc.MetricsHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()
	for _, want := range []string{`jellyfinner_jobs{state="queued"} 2`, "jellyfinner_backfill_remaining_titles 0"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
}

func TestTitleStates(t *testing.T) {
	f := newFixture(t)
	f.library3(1)
	f.write("new.mkv", "n")
	f.scan()
	f.write("live.mkv", "l")
	f.finishTitleAfterScan("live.mkv")

	rows, total, err := f.svc.Titles("", 100)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("total = %d", total)
	}
	states := map[string]string{}
	for _, r := range rows {
		states[r.Path] = r.State
	}
	if states["t0.mkv"] != TitleBacklog || states["live.mkv"] != TitleReady {
		t.Errorf("states = %v", states)
	}
	if _, total, _ := f.svc.Titles("LIVE", 100); total != 1 {
		t.Errorf("search is case-insensitive: %d", total)
	}
}

func (f *fixture) finishTitleAfterScan(source string) {
	f.t.Helper()
	f.stabilise()
	for {
		j := f.claim()
		f.upload(j, map[string]string{filepath.Base(j.OutputPath): "video"})
		f.complete(j)
		f.svc.PublishPending(context.Background())
		if j.Path == source {
			return
		}
	}
}
