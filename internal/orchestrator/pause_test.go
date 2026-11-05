package orchestrator

import (
	"errors"
	"testing"
	"time"

	"jellyfinner/internal/model"
)

func TestPauseNode(t *testing.T) {
	f := newFixture(t)
	for _, n := range []*model.Node{
		{Name: "worker1", Kind: model.NodeWorker, TokenHash: "a"},
		{Name: "worker2", Kind: model.NodeWorker, TokenHash: "b"},
		{Name: "edge1", Kind: model.NodeEdge, TokenHash: "c", URL: "https://e", QuotaBytes: 1},
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
