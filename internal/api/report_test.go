package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"jellyfinner/internal/buildinfo"
	"jellyfinner/internal/control"
)

func TestWorkerReport(t *testing.T) {
	f := newFixture(t, allNodes())
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	f.srv.Now = func() time.Time { return now }
	f.client.Role = "worker"
	info := &control.WorkerInfo{CPU: "Test CPU", Cores: 4, Threads: 8, Backend: "vaapi", FFmpeg: "ffmpeg version 7"}

	// An idle claim records the info and the version.
	if _, err := f.client.Claim(ctx, info); err != nil {
		t.Fatal(err)
	}
	rep := f.srv.NodeReports()["worker1"]
	if rep.Worker == nil || rep.Worker.CPU != "Test CPU" || rep.Worker.Threads != 8 {
		t.Fatalf("worker info = %+v", rep.Worker)
	}
	if rep.Version != buildinfo.String() || rep.Platform == "" || !strings.HasPrefix(rep.UserAgent, "jellyfinner-worker/") {
		t.Errorf("version %q, user agent %q", rep.Version, rep.UserAgent)
	}

	f.enqueue(t, "a")
	claim, err := f.client.Claim(ctx, info)
	if err != nil || claim == nil {
		t.Fatalf("Claim = %+v, %v", claim, err)
	}
	beat := func(stage, backend string) control.NodeReport {
		t.Helper()
		if _, err := f.client.Heartbeat(ctx, "a", control.HeartbeatRequest{Lease: claim.Lease, Stage: stage, Backend: backend}); err != nil {
			t.Fatal(err)
		}
		return f.srv.NodeReports()["worker1"]
	}
	if rep := beat("pull", ""); rep.Job != "a" || rep.Stage != "pull" || !rep.EncodeSince.IsZero() {
		t.Fatalf("pulling: %+v", rep)
	}
	started := now.Add(time.Minute)
	now = started
	if rep := beat("encode", "vaapi"); rep.Backend != "vaapi" || !rep.EncodeSince.Equal(started) {
		t.Fatalf("encoding: %+v", rep)
	}
	// A fall back to software keeps the encode's start.
	now = now.Add(time.Minute)
	if rep := beat("encode", "software"); rep.Backend != "software" || !rep.EncodeSince.Equal(started) {
		t.Fatalf("fell back: %+v", rep)
	}

	if err := f.client.Finish(ctx, "a", control.FinishRequest{Lease: claim.Lease, Manifest: json.RawMessage(`{"job_id":"a"}`)}); err != nil {
		t.Fatal(err)
	}
	rep = f.srv.NodeReports()["worker1"]
	if rep.Job != "" || rep.Backend != "" || rep.Worker == nil {
		t.Errorf("after finish: %+v", rep)
	}
}

func TestParseUserAgent(t *testing.T) {
	for ua, want := range map[string][2]string{
		"jellyfinner-router/1.2.3+sha-abc (linux/arm64; go1.27)": {"1.2.3+sha-abc", "linux/arm64; go1.27"},
		"Go-http-client/1.1": {"", ""},
		"":                   {"", ""},
	} {
		if v, p := control.ParseUserAgent(ua); v != want[0] || p != want[1] {
			t.Errorf("%q: got %q, %q, want %q", ua, v, p, want)
		}
	}
}
