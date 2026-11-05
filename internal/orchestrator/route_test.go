package orchestrator

import (
	"strings"
	"testing"
	"time"

	"jellyfinner/internal/edge"
	"jellyfinner/internal/model"
)

var routeHash = strings.Repeat("a", 64)

// routed is an edge with alice assigned and Movies/A.mp4 active and healthy.
func routed(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	f.cfg.EdgeURLTTL = 24 * time.Hour
	added, err := f.svc.AddNode(NodeSpec{Name: "edge1", Edge: true, URL: "https://edge1.example.com", QuotaGB: 1})
	if err != nil {
		t.Fatal(err)
	}
	id := added.Node.ID
	if _, err := f.svc.AssignViewer("alice", id); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AddPlacement(&model.Placement{Node: id, SHA256: routeHash, Output: "Movies/A.mp4", Size: 5}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ActivatePlacement(id, routeHash); err != nil {
		t.Fatal(err)
	}
	healthy(t, f, id)
	return f, id
}

func healthy(t *testing.T, f *fixture, id string) {
	t.Helper()
	if err := f.store.NodeSeen(id, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetNodeHealth(id, true); err != nil {
		t.Fatal(err)
	}
}

func TestEdgeRedirectSignsAndRecordsThePlay(t *testing.T) {
	f, id := routed(t)
	got, err := f.svc.EdgeRedirect("Alice", "Movies/A.mp4")
	if err != nil || !strings.HasPrefix(got, "https://edge1.example.com/e/"+routeHash+".mp4?md5=") {
		t.Fatalf("redirect = %q, %v", got, err)
	}
	if ps, _ := f.store.NodePlacements(id); len(ps) != 1 || ps[0].LastPlayed == 0 {
		t.Error("the play was not recorded")
	}

	// The viewer follows the edge through a rename and a new URL.
	if _, _, err := f.svc.EditNode(id, NodeEdit{Name: ptr("edge-paris"), URL: ptr("https://example.com/edge1")}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.EdgeRedirect("alice", "Movies/A.mp4"); !strings.HasPrefix(got, "https://example.com/edge1/e/"+routeHash) {
		t.Errorf("after the edit = %q", got)
	}
}

func TestEdgeRedirectFallsBackToTheOrigin(t *testing.T) {
	cases := map[string]func(*fixture, string) (user, output string){
		"no viewer":  func(*fixture, string) (string, string) { return "bob", "Movies/A.mp4" },
		"not cached": func(*fixture, string) (string, string) { return "alice", "Movies/B.mp4" },
		"no output":  func(*fixture, string) (string, string) { return "alice", "" },
		"deleting": func(f *fixture, id string) (string, string) {
			f.store.SetPlacementState(id, routeHash, model.PlacementDeleting, "")
			return "alice", "Movies/A.mp4"
		},
		"failed": func(f *fixture, id string) (string, string) {
			f.store.SetPlacementState(id, routeHash, model.PlacementFailed, "x")
			return "alice", "Movies/A.mp4"
		},
		"probe failing": func(f *fixture, id string) (string, string) {
			f.store.SetNodeHealth(id, false)
			return "alice", "Movies/A.mp4"
		},
		"agent silent": func(f *fixture, _ string) (string, string) {
			f.advance(2 * edge.HealthWindow)
			return "alice", "Movies/A.mp4"
		},
		"edge removed": func(f *fixture, id string) (string, string) {
			f.svc.Nodes.Remove(id)
			return "alice", "Movies/A.mp4"
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f, id := routed(t)
			user, output := setup(f, id)
			if got, err := f.svc.EdgeRedirect(user, output); err != nil || got != "" {
				t.Errorf("redirect = %q, %v, want the origin", got, err)
			}
		})
	}
}
