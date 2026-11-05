package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"jellyfinner/internal/model"
)

func TestPauseNode(t *testing.T) {
	f := newFixture(t)
	for _, n := range []*model.Node{
		{ID: "worker1", Name: "worker1", Kind: model.NodeWorker, TokenHash: "a"},
		{ID: "worker2", Name: "worker2", Kind: model.NodeWorker, TokenHash: "b"},
		{ID: "edge1", Name: "edge1", Kind: model.NodeEdge, TokenHash: "c", URL: "https://e", QuotaBytes: 1},
	} {
		if err := f.svc.Nodes.Add(n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.Enqueue(&model.Job{ID: "a", Path: "a.mkv", OutputPath: "a.mp4", Priority: model.PriorityNew}); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.PauseNode("worker1"); err != nil {
		t.Fatal(err)
	}
	if j, _ := f.store.Claim("worker1", time.Minute); j != nil {
		t.Fatal("a paused worker claimed a job")
	}
	if err := f.svc.PauseNode("edge1"); err == nil {
		t.Error("an edge node takes no jobs, so it cannot be paused")
	}
	if err := f.svc.PauseNode("nobody"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown node: %v", err)
	}
	if err := f.svc.ResumeNode("worker1"); err != nil {
		t.Fatal(err)
	}
	if j, _ := f.store.Claim("worker1", time.Minute); j == nil {
		t.Error("a resumed worker must claim again")
	}

	names, err := f.svc.SetAllWorkersPaused(true)
	if err != nil || len(names) != 2 {
		t.Fatalf("pause all = %v, %v", names, err)
	}
	nodes, _ := f.store.LoadNodes()
	for _, n := range nodes {
		if n.Paused == n.Edge() {
			t.Errorf("%s paused = %v; only workers should be", n.Name, n.Paused)
		}
	}
	if _, err := f.svc.SetAllWorkersPaused(false); err != nil {
		t.Fatal(err)
	}
	nodes, _ = f.store.LoadNodes()
	for _, n := range nodes {
		if n.Paused {
			t.Errorf("%s still paused after resume all", n.Name)
		}
	}
}

func TestRequeueTakesAJobFromItsWorker(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"worker1", "worker2"} {
		if err := f.svc.Nodes.Add(&model.Node{ID: name, Name: name, Kind: model.NodeWorker, TokenHash: name}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.Enqueue(&model.Job{ID: "a", Path: "a.mkv", OutputPath: "a.mp4", Priority: model.PriorityNew}); err != nil {
		t.Fatal(err)
	}
	j, _ := f.store.Claim("worker1", time.Minute)
	if j == nil {
		t.Fatal("no claim")
	}

	// Without pause, the same worker may take it straight back.
	if node, err := f.svc.Requeue("a", false); err != nil || node != "worker1" {
		t.Fatalf("Requeue = %q, %v", node, err)
	}
	if j, _ = f.store.Claim("worker1", time.Minute); j == nil || j.Attempts != 1 {
		t.Fatalf("worker1 could not claim it back: %+v", j)
	}

	// With pause, it cannot, and another worker gets it.
	if _, err := f.svc.Requeue("a", true); err != nil {
		t.Fatal(err)
	}
	if again, _ := f.store.Claim("worker1", time.Minute); again != nil {
		t.Fatal("a worker paused by requeue claimed the job back")
	}
	if other, _ := f.store.Claim("worker2", time.Minute); other == nil || other.ID != "a" {
		t.Errorf("worker2 claim = %+v", other)
	}

	if _, err := f.svc.Requeue("missing", true); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown job: %v", err)
	}
	if err := f.svc.ResumeNode("worker1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Requeue("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Requeue("a", true); err == nil {
		t.Error("requeued a queued job")
	}
	if nodes, _ := f.store.LoadNodes(); nodes[0].Paused || nodes[1].Paused {
		t.Errorf("a refused requeue must not pause anyone: %+v", nodes)
	}
}

func TestRequeueWithPauseDrainsACloudServer(t *testing.T) {
	cf := newCloud(t, "alpha")
	r, err := cf.cloud.Start(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	cf.seen[r.Node] = cf.now
	cf.queue("a", model.PriorityNew)
	if j, _ := cf.store.Claim(r.Node, time.Hour); j == nil {
		t.Fatal("the cloud server could not claim")
	}
	cf.tick()
	if got := cf.servers()[0]; got.State != model.CloudReady {
		t.Fatalf("before requeue: %+v", got)
	}
	if _, err := cf.svc.Requeue("a", true); err != nil {
		t.Fatal(err)
	}
	cf.tick()
	if got := cf.servers()[0]; got.State != model.CloudDraining && got.State != model.CloudDeleting {
		t.Errorf("a paused cloud server must drain: %+v", got)
	}
}
