package web

import (
	"strings"
	"testing"
	"time"

	"jellyfinner/internal/buildinfo"
	"jellyfinner/internal/control"
	"jellyfinner/internal/model"
)

func TestJobPageShowsTimes(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.Enqueue(&model.Job{ID: "ep1", Path: "ep1.mkv", OutputPath: "ep1.mp4", Priority: model.PriorityNew}); err != nil {
		t.Fatal(err)
	}
	c := f.admin()
	if _, body := f.get(c, "/jobs/ep1"); !strings.Contains(body, "Last attempt") || !strings.Contains(body, "<dt>Started</dt><dd>—</dd>") {
		t.Errorf("a queued job should show no start time")
	}

	started := f.now
	j, err := f.store.Claim("worker1", time.Hour)
	if err != nil || j == nil {
		t.Fatalf("claim = %v, %v", j, err)
	}
	f.now = started.Add(10 * time.Minute)
	if _, body := f.get(c, "/jobs/ep1"); !strings.Contains(body, "still running") || !strings.Contains(body, "10m 00s") {
		t.Errorf("a running job should show its time so far")
	}

	f.now = started.Add(90 * time.Minute)
	encStart := started.Add(5 * time.Minute).UTC()
	manifest := `{"job_id":"ep1","backend":"vaapi","video_copy":false,"files":[],"timing":{` +
		`"encode_started":"` + encStart.Format(time.RFC3339) + `","encode_finished":"` + encStart.Add(80*time.Minute).Format(time.RFC3339) +
		`","encode_seconds":4800,"pull_seconds":65,"push_seconds":30}}`
	if err := f.store.Complete(j.ID, j.LeaseID, manifest, ""); err != nil {
		t.Fatal(err)
	}
	_, body := f.get(c, "/jobs/ep1")
	for _, want := range []string{
		started.Local().Format("2006-01-02 15:04:05"),
		started.Add(90 * time.Minute).Local().Format("2006-01-02 15:04:05"),
		"1h 30m 00s",
		encStart.Local().Format("2006-01-02 15:04:05"),
		"1h 20m 00s",
		"1m 05s",
		"hardware (vaapi)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("job page missing %q", want)
		}
	}
}

func TestNodesPageShowsWorkerReport(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.Enqueue(&model.Job{ID: "ep1", Path: "ep1.mkv", OutputPath: "ep1.mp4", Priority: model.PriorityNew}); err != nil {
		t.Fatal(err)
	}
	if j, err := f.store.Claim("worker1", time.Hour); err != nil || j == nil {
		t.Fatalf("claim = %v, %v", j, err)
	}
	f.reports = map[string]control.NodeReport{"worker1": {
		Version: "1.2.3+sha-abc1234",
		Worker:  &control.WorkerInfo{CPU: "Test CPU 9000", Cores: 8, Threads: 16, CPULimit: 4, Backend: "vaapi", FFmpeg: "ffmpeg version 7.1-Jellyfin"},
		Job:     "ep1", Stage: "encode", Backend: "software", EncodeSince: f.now,
	}}
	c := f.admin()
	_, body := f.get(c, "/nodes")
	for _, want := range []string{escapePlus("1.2.3+sha-abc1234"), "Test CPU 9000", "8C/16T", "limit 4 CPUs", "hardware: vaapi",
		"ffmpeg version 7.1-Jellyfin", "software (fallback)", "encoding since", "Jellyfinner " + escapePlus(buildinfo.String())} {
		if !strings.Contains(body, want) {
			t.Errorf("nodes page missing %q", want)
		}
	}

	// Without a fall back, the live encoder shows as hardware.
	rep := f.reports["worker1"]
	rep.Backend = "vaapi"
	f.reports["worker1"] = rep
	if _, body := f.get(c, "/fragments/nodes/workers"); !strings.Contains(body, "hardware (vaapi)") || strings.Contains(body, "fallback") {
		t.Errorf("workers fragment should show hardware (vaapi) without fallback")
	}
}

// escapePlus matches html/template, which writes + as &#43; in text.
func escapePlus(s string) string { return strings.ReplaceAll(s, "+", "&#43;") }

func TestNodesPageShowsEveryNodesBuild(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.Nodes.Add(&model.Node{ID: "edge1", Name: "edge1", Kind: model.NodeEdge, TokenHash: "xe", URL: "https://edge1.example.com", QuotaBytes: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Nodes.Add(&model.Node{ID: "router1", Name: "router1", Kind: model.NodeRouter, TokenHash: "xr"}); err != nil {
		t.Fatal(err)
	}
	f.reports = map[string]control.NodeReport{
		"edge1":   {UserAgent: "jellyfinner-edge-agent/2.0.0 (linux/amd64; go1.27)"},
		"router1": {UserAgent: "jellyfinner-router/3.0.0 (linux/arm64; go1.27)"},
	}
	_, body := f.get(f.admin(), "/nodes")
	for _, want := range []string{"2.0.0", "linux/amd64; go1.27", "3.0.0", "linux/arm64; go1.27", "Pull agent:", "File server:", "version unknown"} {
		if !strings.Contains(body, want) {
			t.Errorf("nodes page missing %q", want)
		}
	}
}
