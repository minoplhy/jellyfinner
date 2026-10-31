package queue

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Node kinds. A node has exactly one, so an edge token can never take a job.
const (
	NodeWorker = "worker"
	NodeEdge   = "edge"
)

// nodeCacheFor bounds how long a removed node keeps working, and how long a new
// one waits. Every request needs this list, and the router is another process.
const nodeCacheFor = 10 * time.Second

// ErrNodeExists means the name is taken.
var ErrNodeExists = errors.New("node already exists")

// Node is a worker or edge node allowed on the control API. An empty Allow list
// accepts any source address. URL, QuotaBytes and SigningKey are edge only.
type Node struct {
	Name       string
	Kind       string
	TokenHash  string
	Allow      []netip.Prefix
	URL        string
	QuotaBytes int64
	SigningKey string
}

// Edge reports whether this node caches encodes.
func (n *Node) Edge() bool { return n.Kind == NodeEdge }

type nodeCache struct {
	mu    sync.Mutex
	nodes []Node
	until time.Time
}

// NewSecret returns a random token or key.
func NewSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// HashToken is what the database keeps instead of a token. Tokens are long and
// random, so a plain hash is enough to make a stolen database useless here.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// EnsureSecret returns a stored secret, creating it on first use.
func (s *Store) EnsureSecret(key string) (string, error) {
	fresh, err := NewSecret()
	if err != nil {
		return "", err
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO meta (key, value) VALUES (?, ?)`, key, fresh); err != nil {
		return "", err
	}
	return s.Meta(key)
}

// AddNode stores a new node.
func (s *Store) AddNode(n *Node) error {
	allow := make([]string, len(n.Allow))
	for i, prefix := range n.Allow {
		allow[i] = prefix.String()
	}
	res, err := s.db.Exec(`INSERT OR IGNORE INTO nodes (name, kind, token_hash, allow, url, quota_bytes, signing_key, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		n.Name, n.Kind, n.TokenHash, strings.Join(allow, ","), n.URL, n.QuotaBytes, n.SigningKey, s.Now().Unix())
	if err != nil {
		return err
	}
	if added, err := res.RowsAffected(); err != nil {
		return err
	} else if added == 0 {
		return fmt.Errorf("%s: %w", n.Name, ErrNodeExists)
	}
	s.forgetNodes()
	return nil
}

// RemoveNode revokes a node and forgets what it cached. The files stay on the
// node, because nothing can order it to delete them any more.
func (s *Store) RemoveNode(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM nodes WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if removed, err := res.RowsAffected(); err != nil {
		return err
	} else if removed == 0 {
		return fmt.Errorf("node %s: %w", name, ErrNotFound)
	}
	if _, err := tx.Exec(`DELETE FROM placements WHERE node = ?`, name); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM edge_status WHERE node = ?`, name); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.forgetNodes()
	return nil
}

// SetNodeToken replaces a node's token, which revokes the old one.
func (s *Store) SetNodeToken(name, tokenHash string) error {
	res, err := s.db.Exec(`UPDATE nodes SET token_hash = ? WHERE name = ?`, tokenHash, name)
	if err != nil {
		return err
	}
	if changed, err := res.RowsAffected(); err != nil {
		return err
	} else if changed == 0 {
		return fmt.Errorf("node %s: %w", name, ErrNotFound)
	}
	s.forgetNodes()
	return nil
}

// LoadNodes reads every node from the database, by name.
func (s *Store) LoadNodes() ([]Node, error) {
	rows, err := s.db.Query(`SELECT name, kind, token_hash, allow, url, quota_bytes, signing_key FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []Node
	for rows.Next() {
		var n Node
		var allow string
		if err := rows.Scan(&n.Name, &n.Kind, &n.TokenHash, &allow, &n.URL, &n.QuotaBytes, &n.SigningKey); err != nil {
			return nil, err
		}
		if n.Allow, err = parseAllow(allow); err != nil {
			return nil, fmt.Errorf("node %s: %w", n.Name, err)
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

func parseAllow(s string) ([]netip.Prefix, error) {
	if s == "" {
		return nil, nil
	}
	var out []netip.Prefix
	for _, cidr := range strings.Split(s, ",") {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return nil, err
		}
		out = append(out, prefix)
	}
	return out, nil
}

func (s *Store) forgetNodes() {
	s.nodes.mu.Lock()
	defer s.nodes.mu.Unlock()
	s.nodes.until = time.Time{}
}

// Nodes returns the node list, at most nodeCacheFor old. If the database cannot
// be read, the last good list stays in use.
func (s *Store) Nodes() []Node {
	s.nodes.mu.Lock()
	defer s.nodes.mu.Unlock()
	if now := s.Now(); now.After(s.nodes.until) {
		if nodes, err := s.LoadNodes(); err == nil {
			s.nodes.nodes = nodes
			s.nodes.until = now.Add(nodeCacheFor)
		}
	}
	return s.nodes.nodes
}

// EdgeNodes returns the nodes that may cache encodes.
func (s *Store) EdgeNodes() []Node {
	var out []Node
	for _, n := range s.Nodes() {
		if n.Edge() {
			out = append(out, n)
		}
	}
	return out
}

// EdgeNode finds an edge node by name.
func (s *Store) EdgeNode(name string) *Node {
	for _, n := range s.EdgeNodes() {
		if n.Name == name {
			return &n
		}
	}
	return nil
}

// NodeByToken checks every node, so timing does not reveal which one matched.
func (s *Store) NodeByToken(token string) *Node {
	hash := []byte(HashToken(token))
	var found *Node
	nodes := s.Nodes()
	for i := range nodes {
		if subtle.ConstantTimeCompare(hash, []byte(nodes[i].TokenHash)) == 1 {
			found = &nodes[i]
		}
	}
	return found
}
