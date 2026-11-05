package nodes

import (
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"jellyfinner/internal/database"
	"jellyfinner/internal/database/sqlite"
	. "jellyfinner/internal/model"
)

// fixture is a registry over a real database, and the database itself.
type fixture struct {
	*Registry
	db database.Store
}

func open(t *testing.T, path string, now func() time.Time) fixture {
	t.Helper()
	db, err := sqlite.Open(path, database.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return fixture{Registry: New(db, now), db: db}
}

func openNodes(t *testing.T) (fixture, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	return open(t, path, nil), path
}

func TestNodeLifecycle(t *testing.T) {
	s, _ := openNodes(t)
	allow := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.0.2.1/32")}
	if err := s.Add(&Node{ID: "worker1", Name: "worker1", Kind: NodeWorker, TokenHash: HashToken("one"), Allow: allow}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(&Node{ID: "edge1", Name: "edge1", Kind: NodeEdge, TokenHash: HashToken("two"), URL: "https://e", QuotaBytes: 5, SigningKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(&Node{ID: "worker1", Name: "worker1", Kind: NodeWorker, TokenHash: HashToken("three")}); !errors.Is(err, ErrNodeExists) {
		t.Errorf("duplicate: %v", err)
	}

	if n := s.ByToken("one"); n == nil || n.Name != "worker1" || len(n.Allow) != 2 || n.Allow[1] != allow[1] {
		t.Errorf("token one: %+v", n)
	}
	if n := s.ByToken("wrong"); n != nil {
		t.Errorf("a wrong token matched %+v", n)
	}
	if n := s.ByToken(""); n != nil {
		t.Errorf("an empty token matched %+v", n)
	}
	if s.Edge("worker1") != nil {
		t.Error("a worker is not an edge node")
	}
	if n := s.Edge("edge1"); n == nil || n.QuotaBytes != 5 || n.SigningKey != "k" {
		t.Errorf("edge1: %+v", n)
	}

	if err := s.SetToken("worker1", HashToken("rotated")); err != nil {
		t.Fatal(err)
	}
	if s.ByToken("one") != nil || s.ByToken("rotated") == nil {
		t.Error("rotating must replace the token")
	}
	if err := s.SetToken("nobody", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rotate unknown: %v", err)
	}
}

func TestRemoveNodeForgetsPlacements(t *testing.T) {
	s, _ := openNodes(t)
	if err := s.Add(&Node{ID: "edge1", Name: "edge1", Kind: NodeEdge, TokenHash: HashToken("t")}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.AddPlacement(&Placement{Node: "edge1", SHA256: "abc", Output: "a.mp4", Size: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("edge1"); err != nil {
		t.Fatal(err)
	}
	if s.ByToken("t") != nil {
		t.Error("a removed node must lose access")
	}
	placements, err := s.db.NodePlacements("edge1")
	if err != nil || len(placements) != 0 {
		t.Errorf("placements = %v, %v", placements, err)
	}
	if err := s.Remove("edge1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("remove twice: %v", err)
	}
}

// The CLI and the router are other processes, so only the cache's age brings
// their changes into this one.
func TestOtherProcessChangesArriveWithTheCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	now := time.Now()
	s := open(t, path, func() time.Time { return now })
	other := open(t, path, nil)
	if err := other.Add(&Node{ID: "worker1", Name: "worker1", Kind: NodeWorker, TokenHash: HashToken("t")}); err != nil {
		t.Fatal(err)
	}
	if s.ByToken("t") == nil {
		t.Fatal("an empty cache must load")
	}
	if err := other.Remove("worker1"); err != nil {
		t.Fatal(err)
	}
	if s.ByToken("t") == nil {
		t.Error("the cache should still hold the node")
	}
	now = now.Add(cacheFor + time.Second)
	if s.ByToken("t") != nil {
		t.Error("a removed node must stop working once the cache ages out")
	}
}

// An edit in another process, such as the CLI, reaches the router with the
// cache: a new URL is used for the next redirect, and the edge is found by
// its new name while its ID stays.
func TestEditArrivesWithTheCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	now := time.Now()
	s := open(t, path, func() time.Time { return now })
	other := open(t, path, nil)
	id, err := NewID()
	if err != nil || len(id) != 16 {
		t.Fatalf("NewID = %q, %v", id, err)
	}
	if err := other.Add(&Node{ID: id, Name: "edge1", Kind: NodeEdge, TokenHash: HashToken("t"), URL: "https://old.example"}); err != nil {
		t.Fatal(err)
	}
	if n := s.EdgeByName("edge1"); n == nil || n.ID != id {
		t.Fatalf("EdgeByName = %+v", n)
	}
	if err := other.Update(&Node{ID: id, Name: "edge-paris", URL: "https://example.com/edge1"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(cacheFor + time.Second)
	if s.EdgeByName("edge1") != nil {
		t.Error("the old name still finds the edge")
	}
	if n := s.Edge(id); n == nil || n.Name != "edge-paris" || n.URL != "https://example.com/edge1" {
		t.Errorf("Edge(id) = %+v", n)
	}
	if n := s.ByID(id); n == nil || s.ByToken("t") == nil {
		t.Error("the token must keep working through an edit")
	}
}

func TestEnsureSecretSurvivesRestart(t *testing.T) {
	s, path := openNodes(t)
	first, err := EnsureSecret(s.db, "edge_token_key")
	if err != nil || len(first) < 32 {
		t.Fatalf("secret %q, %v", first, err)
	}
	s.db.Close()
	again := open(t, path, nil)
	if second, err := EnsureSecret(again.db, "edge_token_key"); err != nil || second != first {
		t.Errorf("second = %q, %v, want %q", second, err, first)
	}
}
