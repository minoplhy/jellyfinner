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
	f.cfg.EdgeViewers = map[string]string{"alice": "edge1", "bob": "edge1", "carol": "other"}
	added, err := f.svc.AddNode(NodeSpec{Name: "edge1", Edge: true, URL: "https://edge1.example.com", QuotaGB: 10})
	if err != nil {
		t.Fatal(err)
	}
	id := added.Node.ID
	if err := f.store.AddPlacement(&model.Placement{Node: id, SHA256: strings.Repeat("a", 64), Output: "a.mp4", Size: 3e9}); err != nil {
		t.Fatal(err)
	}

	n, warnings, err := f.svc.EditNode(id, NodeEdit{Name: ptr("edge-paris"), URL: ptr("https://example.com/edge1/"), QuotaGB: ptr(int64(2))})
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != id || n.Name != "edge-paris" || n.URL != "https://example.com/edge1" || n.QuotaBytes != 2e9 || n.SigningKey != added.SigningKey {
		t.Errorf("edited = %+v", n)
	}
	if len(warnings) != 3 || !strings.Contains(warnings[0], "alice") || !strings.Contains(warnings[1], "bob") ||
		!strings.Contains(warnings[2], "more than the new quota") {
		t.Errorf("warnings = %q", warnings)
	}
	// Placements follow the ID through the rename.
	rows, err := f.svc.NodeRows()
	if err != nil || len(rows) != 1 || rows[0].Name != "edge-paris" || rows[0].Used != 3e9 || len(rows[0].Placements) != 1 {
		t.Errorf("rows = %+v, %v", rows, err)
	}

	if _, _, err := f.svc.EditNode(id, NodeEdit{URL: ptr("http://plain.example.com")}); err == nil {
		t.Error("an edge URL must be https")
	}
	if _, _, err := f.svc.EditNode(id, NodeEdit{QuotaGB: ptr(int64(0))}); err == nil {
		t.Error("a quota must be positive")
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
