package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"jellyfinner/internal/model"
)

func TestUsagePage(t *testing.T) {
	f := newFixture(t)
	f.now = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	ok, err := f.store.Enqueue(&model.Job{ID: "ep1", Path: "ep1.mkv", OutputPath: "ep1.mp4", Priority: model.PriorityNew, SourceSize: 2e9})
	if err != nil || !ok {
		t.Fatal(err)
	}
	j, err := f.store.Claim("worker1", time.Hour)
	if err != nil || j == nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Hour)
	if err := f.store.Complete(j.ID, j.LeaseID, `{"video_copy":false,"files":[{"size":500000000}]}`, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkDone(j.ID); err != nil {
		t.Fatal(err)
	}
	f.now = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	c := f.admin()

	code, body := f.get(c, "/usage")
	if code != http.StatusOK {
		t.Fatalf("GET /usage = %d", code)
	}
	for _, want := range []string{"November 2026", `href="/usage?month=2026-10"`, "No job finished this month.", "Backlog forecast",
		`hx-get="/fragments/usage"`} {
		if !strings.Contains(body, want) {
			t.Errorf("current month lacks %q", want)
		}
	}
	// No cloud provider is listed, so no cloud parts.
	if strings.Contains(body, "Cloud spending") || strings.Contains(body, "How each provider bills") {
		t.Error("cloud parts shown with no provider")
	}

	code, body = f.get(c, "/usage?month=2026-10")
	if code != http.StatusOK {
		t.Fatalf("October = %d", code)
	}
	for _, want := range []string{"October 2026", `href="/usage?month=2026-11"`, "<td>worker1</td>", `2.00 <span class="muted">(1 timed)</span>`,
		"500 MB"} {
		if !strings.Contains(body, want) {
			t.Errorf("October lacks %q", want)
		}
	}
	if strings.Contains(body, "rel=\"prev\"") || strings.Contains(body, "data-poll") {
		t.Error("October is the first month and is not live")
	}

	for _, bad := range []string{"/usage?month=bad", "/usage?month=2027-01", "/fragments/usage?month=13-2026"} {
		if code, _ := f.get(c, bad); code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", bad, code)
		}
	}
	if code, body := f.get(c, "/fragments/usage"); code != http.StatusOK || !strings.Contains(body, `id="usage-body"`) {
		t.Errorf("fragment = %d", code)
	}
}
