package orchestrator

import (
	"strings"
	"testing"
	"time"

	"jellyfinner/internal/model"
)

func ptr[T any](v T) *T { return &v }

func TestEditWorker(t *testing.T) {
	f := newFixture(t)
	added, err := f.svc.AddNode(NodeSpec{Name: "worker1"})
	if err != nil {
		t.Fatal(err)
	}
	id := added.Node.ID
	if len(id) != 16 {
		t.Fatalf("ID = %q", id)
	}
	f.runJob("a", id, 1e9, 0, 30*time.Minute, `{}`)

	n, warnings, err := f.svc.EditNode(id, NodeEdit{Name: ptr("gpu-box"), Allow: ptr("10.8.0.0/24, 203.0.113.7/32")})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("edit = %v, %v", warnings, err)
	}
	if n.ID != id || n.Name != "gpu-box" || AllowString(n.Allow) != "10.8.0.0/24, 203.0.113.7/32" {
		t.Errorf("edited = %+v", n)
	}
	// Same token, same node: the worker needs no change.
	if got := f.svc.Nodes.ByToken(added.Token); got == nil || got.ID != id || got.Name != "gpu-box" {
		t.Errorf("by token = %+v", got)
	}
	// History follows the ID, so the earlier job shows the new name, and
	// usage keeps one row for the worker.
	if j, _ := f.store.Get("a"); j.NodeName != "gpu-box" {
		t.Errorf("old job's node = %q", j.NodeName)
	}
	f.runJob("b", id, 1e9, 0, 30*time.Minute, `{}`)
	s := f.summary(f.now, nil)
	if len(s.Work) != 1 || s.Work[0].Name != "gpu-box" || s.Work[0].Published != 2 {
		t.Errorf("work = %+v", s.Work)
	}

	// An empty list accepts any address again.
	if n, _, err = f.svc.EditNode(id, NodeEdit{Allow: ptr("")}); err != nil || len(n.Allow) != 0 {
		t.Errorf("clearing allow = %+v, %v", n, err)
	}

	for _, c := range []struct {
		edit NodeEdit
		want string
	}{
		{NodeEdit{}, "nothing to change"},
		{NodeEdit{URL: ptr("https://x")}, "only for edge nodes"},
		{NodeEdit{QuotaGB: ptr(int64(5))}, "only for edge nodes"},
		{NodeEdit{Allow: ptr("not-a-cidr")}, ""},
		{NodeEdit{Name: ptr("Bad Name")}, "lowercase"},
		{NodeEdit{Name: ptr("cloud-mine")}, "rented servers"},
	} {
		if _, _, err := f.svc.EditNode(id, c.edit); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.edit, err, c.want)
		}
	}
	if _, err := f.svc.AddNode(NodeSpec{Name: "worker2"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.EditNode(id, NodeEdit{Name: ptr("worker2")}); !strings.Contains(errText(err), "exists") {
		t.Errorf("taking another node's name: %v", err)
	}
	if _, _, err := f.svc.EditNode("nobody", NodeEdit{Name: ptr("x")}); err == nil {
		t.Error("editing an unknown node must fail")
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestEditEdgeNode(t *testing.T) {
	f := newFixture(t)
	added, err := f.svc.AddNode(NodeSpec{Name: "edge1", Edge: true, URL: "https://edge1.example.com", QuotaGB: 10})
	if err != nil {
		t.Fatal(err)
	}
	id := added.Node.ID
	if err := f.store.AddPlacement(&model.Placement{Node: id, SHA256: strings.Repeat("a", 64), Output: "a.mp4", Size: 3e9}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AssignViewer("alice", id); err != nil {
		t.Fatal(err)
	}

	n, warnings, err := f.svc.EditNode(id, NodeEdit{Name: ptr("edge-paris"), URL: ptr("https://example.com/edge1/"), QuotaGB: ptr(int64(2))})
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != id || n.Name != "edge-paris" || n.URL != "https://example.com/edge1" || n.QuotaBytes != 2e9 || n.SigningKey != added.SigningKey {
		t.Errorf("edited = %+v", n)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "more than the new quota") {
		t.Errorf("warnings = %q", warnings)
	}
	// Placements and viewers follow the ID through the rename.
	rows, err := f.svc.NodeRows()
	if err != nil || len(rows) != 1 || rows[0].Name != "edge-paris" || rows[0].Used != 3e9 || len(rows[0].Placements) != 1 ||
		len(rows[0].Viewers) != 1 || rows[0].Viewers[0] != "alice" {
		t.Errorf("rows = %+v, %v", rows, err)
	}

	if _, _, err := f.svc.EditNode(id, NodeEdit{URL: ptr("http://plain.example.com")}); err == nil {
		t.Error("an edge URL must be https")
	}
	if _, _, err := f.svc.EditNode(id, NodeEdit{QuotaGB: ptr(int64(0))}); err == nil {
		t.Error("a quota must be positive")
	}
}

func TestAssignViewers(t *testing.T) {
	f := newFixture(t)
	edge1, err := f.svc.AddNode(NodeSpec{Name: "edge1", Edge: true, URL: "https://e1.example.com", QuotaGB: 1})
	if err != nil {
		t.Fatal(err)
	}
	edge2, err := f.svc.AddNode(NodeSpec{Name: "edge2", Edge: true, URL: "https://e2.example.com", QuotaGB: 1})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := f.svc.AddNode(NodeSpec{Name: "worker1"})
	if err != nil {
		t.Fatal(err)
	}
	if from, err := f.svc.AssignViewer(" alice ", edge1.Node.ID); err != nil || from != "" {
		t.Fatalf("assign = %q, %v", from, err)
	}
	if from, err := f.svc.AssignViewer("Alice", edge2.Node.ID); err != nil || from != "edge1" {
		t.Errorf("move = %q, %v", from, err)
	}
	if got := f.svc.EdgeViewers(edge1.Node.ID); len(got) != 0 {
		t.Errorf("edge1 viewers after the move = %v", got)
	}
	if got := f.svc.EdgeViewers(edge2.Node.ID); len(got) != 1 || got[0] != "Alice" {
		t.Errorf("edge2 viewers = %v", got)
	}
	for _, c := range []struct{ user, node, want string }{
		{"", edge1.Node.ID, "username"},
		{"bob", worker.Node.ID, "only be sent to an edge"},
		{"bob", "nowhere", "not found"},
	} {
		if _, err := f.svc.AssignViewer(c.user, c.node); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("assign %q to %s: %v, want %q", c.user, c.node, err, c.want)
		}
	}
	if err := f.svc.UnassignViewer("ALICE"); err != nil {
		t.Errorf("unassign: %v", err)
	}
	if err := f.svc.UnassignViewer("alice"); err == nil {
		t.Error("unassigning twice must fail")
	}
	// Removing an edge sends its viewers back to the origin.
	if _, err := f.svc.AssignViewer("carol", edge1.Node.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Nodes.Remove(edge1.Node.ID); err != nil {
		t.Fatal(err)
	}
	if v := f.svc.Nodes.Viewers(); len(v) != 0 {
		t.Errorf("viewers after removing their edge = %+v", v)
	}
}

func TestRentedNodesCannotBeEdited(t *testing.T) {
	f := newFixture(t)
	added, err := f.svc.addCloudNode("cloud-alpha-aa")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.EditNode(added.Node.ID, NodeEdit{Name: ptr("mine")}); err == nil || !strings.Contains(err.Error(), "cloud scaler") {
		t.Errorf("editing a rented node: %v", err)
	}
	if _, err := f.svc.AddNode(NodeSpec{Name: "cloud-alpha-bb"}); err == nil {
		t.Error("a manual node must not take a cloud- name")
	}
}
