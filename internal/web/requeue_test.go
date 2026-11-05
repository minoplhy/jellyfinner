package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"jellyfinner/internal/model"
)

func TestRequeueFromTheWebUI(t *testing.T) {
	f := newFixture(t)
	c := f.admin()
	token := f.csrf(c)
	if _, err := f.store.Enqueue(&model.Job{ID: "ep1", Path: "ep1.mkv", OutputPath: "ep1.mp4", Priority: model.PriorityNew}); err != nil {
		t.Fatal(err)
	}
	if _, body := f.get(c, "/jobs/ep1"); strings.Contains(body, "/jobs/ep1/requeue") {
		t.Error("a queued job has nothing to take back")
	}
	if j, _ := f.store.Claim("worker1", time.Minute); j == nil {
		t.Fatal("no claim")
	}
	if _, body := f.get(c, "/jobs/ep1"); !strings.Contains(body, "/jobs/ep1/requeue") || !strings.Contains(body, `name="pause"`) {
		t.Fatal("a running job's page must offer Requeue with the pause option")
	}

	if code, _ := f.post(c, "/jobs/ep1/requeue", url.Values{"csrf": {token}, "from": {"job"}}); code != http.StatusSeeOther {
		t.Fatalf("requeue = %d", code)
	}
	if j, _ := f.store.Get("ep1"); j.State != model.StateQueued || j.Attempts != 0 {
		t.Errorf("after requeue: %+v", j)
	}
	if _, body := f.get(c, "/jobs/ep1"); !strings.Contains(body, "Taken from worker1 and queued again.") {
		t.Error("missing the requeue message")
	}

	if j, _ := f.store.Claim("worker1", time.Minute); j == nil {
		t.Fatal("worker1 could not claim it back")
	}
	if code, _ := f.post(c, "/jobs/ep1/requeue", url.Values{"csrf": {token}, "pause": {"on"}}); code != http.StatusSeeOther {
		t.Fatalf("requeue with pause = %d", code)
	}
	if nodes, _ := f.store.LoadNodes(); len(nodes) != 1 || !nodes[0].Paused {
		t.Errorf("worker1 must be paused: %+v", nodes)
	}
	if _, body := f.get(c, "/queue"); !strings.Contains(body, "worker1 is paused until you resume it.") {
		t.Error("missing the pause message")
	}

	// A job that is not running is refused with a message, not a crash.
	if code, _ := f.post(c, "/jobs/ep1/requeue", url.Values{"csrf": {token}}); code != http.StatusSeeOther {
		t.Fatalf("requeue of a queued job = %d", code)
	}
	if _, body := f.get(c, "/queue"); !strings.Contains(body, "not running") {
		t.Error("missing the refusal")
	}
}
