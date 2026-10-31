package queue

import (
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
	"time"
)

func openNodes(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestNodeLifecycle(t *testing.T) {
	s, _ := openNodes(t)
	allow := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.0.2.1/32")}
	if err := s.AddNode(&Node{Name: "worker1", Kind: NodeWorker, TokenHash: HashToken("one"), Allow: allow}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddNode(&Node{Name: "edge1", Kind: NodeEdge, TokenHash: HashToken("two"), URL: "https://e", QuotaBytes: 5, SigningKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddNode(&Node{Name: "worker1", Kind: NodeWorker, TokenHash: HashToken("three")}); !errors.Is(err, ErrNodeExists) {
		t.Errorf("duplicate: %v", err)
	}

	if n := s.NodeByToken("one"); n == nil || n.Name != "worker1" || len(n.Allow) != 2 || n.Allow[1] != allow[1] {
		t.Errorf("token one: %+v", n)
	}
	if n := s.NodeByToken("wrong"); n != nil {
		t.Errorf("a wrong token matched %+v", n)
	}
	if n := s.NodeByToken(""); n != nil {
		t.Errorf("an empty token matched %+v", n)
	}
	if s.EdgeNode("worker1") != nil {
		t.Error("a worker is not an edge node")
	}
	if n := s.EdgeNode("edge1"); n == nil || n.QuotaBytes != 5 || n.SigningKey != "k" {
		t.Errorf("edge1: %+v", n)
	}

	if err := s.SetNodeToken("worker1", HashToken("rotated")); err != nil {
		t.Fatal(err)
	}
	if s.NodeByToken("one") != nil || s.NodeByToken("rotated") == nil {
		t.Error("rotating must replace the token")
	}
	if err := s.SetNodeToken("nobody", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rotate unknown: %v", err)
	}
}

func TestRemoveNodeForgetsPlacements(t *testing.T) {
	s, _ := openNodes(t)
	if err := s.AddNode(&Node{Name: "edge1", Kind: NodeEdge, TokenHash: HashToken("t")}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPlacement(&Placement{Node: "edge1", SHA256: "abc", Output: "a.mp4", Size: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveNode("edge1"); err != nil {
		t.Fatal(err)
	}
	if s.NodeByToken("t") != nil {
		t.Error("a removed node must lose access")
	}
	placements, err := s.NodePlacements("edge1")
	if err != nil || len(placements) != 0 {
		t.Errorf("placements = %v, %v", placements, err)
	}
	if err := s.RemoveNode("edge1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("remove twice: %v", err)
	}
}

// The CLI and the router are other processes, so only the cache's age brings
// their changes into this one.
func TestOtherProcessChangesArriveWithTheCache(t *testing.T) {
	s, path := openNodes(t)
	now := time.Now()
	s.Now = func() time.Time { return now }

	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.AddNode(&Node{Name: "worker1", Kind: NodeWorker, TokenHash: HashToken("t")}); err != nil {
		t.Fatal(err)
	}
	if s.NodeByToken("t") == nil {
		t.Fatal("an empty cache must load")
	}
	if err := other.RemoveNode("worker1"); err != nil {
		t.Fatal(err)
	}
	if s.NodeByToken("t") == nil {
		t.Error("the cache should still hold the node")
	}
	now = now.Add(nodeCacheFor + time.Second)
	if s.NodeByToken("t") != nil {
		t.Error("a removed node must stop working once the cache ages out")
	}
}

func TestEnsureSecretSurvivesRestart(t *testing.T) {
	s, path := openNodes(t)
	first, err := s.EnsureSecret("edge_token_key")
	if err != nil || len(first) < 32 {
		t.Fatalf("secret %q, %v", first, err)
	}
	s.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if second, err := again.EnsureSecret("edge_token_key"); err != nil || second != first {
		t.Errorf("second = %q, %v, want %q", second, err, first)
	}
}
