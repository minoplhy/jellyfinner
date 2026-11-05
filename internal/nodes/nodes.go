// Package nodes is the list of workers and edge nodes allowed on the control
// API. It reads through a short cache, because every request needs the list
// and the router runs in another process. Storage is a database.NodeStore.
package nodes

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"sync"
	"time"

	"jellyfinner/internal/database"
	"jellyfinner/internal/model"
)

// cacheFor bounds how long a removed node keeps working, and how long a new
// one waits.
const cacheFor = 10 * time.Second

// Registry caches the node list in front of the database.
type Registry struct {
	store database.NodeStore
	now   func() time.Time

	mu    sync.Mutex
	nodes []model.Node
	until time.Time
}

// New returns a Registry. A nil clock means time.Now.
func New(store database.NodeStore, now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{store: store, now: now}
}

// NewSecret returns a random token or key.
func NewSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// NewID returns a node ID: 16 hex characters that never change, so the name
// can be edited without touching anything that refers to the node.
func NewID() (string, error) {
	b := make([]byte, 8)
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
func EnsureSecret(meta database.MetaStore, key string) (string, error) {
	fresh, err := NewSecret()
	if err != nil {
		return "", err
	}
	return meta.EnsureMeta(key, fresh)
}

// Add stores a new node.
func (r *Registry) Add(n *model.Node) error {
	if err := r.store.AddNode(n); err != nil {
		return err
	}
	r.forget()
	return nil
}

// Remove revokes a node and forgets what it cached. The files stay on the
// node, because nothing can order it to delete them any more.
func (r *Registry) Remove(id string) error {
	if err := r.store.RemoveNode(id); err != nil {
		return err
	}
	r.forget()
	return nil
}

// Update saves a node's name, allowed addresses, URL and quota.
func (r *Registry) Update(n *model.Node) error {
	if err := r.store.UpdateNode(n); err != nil {
		return err
	}
	r.forget()
	return nil
}

// SetToken replaces a node's token, which revokes the old one.
func (r *Registry) SetToken(id, tokenHash string) error {
	if err := r.store.SetNodeToken(id, tokenHash); err != nil {
		return err
	}
	r.forget()
	return nil
}

// SetPaused pauses or resumes a node. Claim reads it from the database, so it
// holds at once; the cache only affects what the UI shows.
func (r *Registry) SetPaused(id string, paused bool) error {
	if err := r.store.SetNodePaused(id, paused); err != nil {
		return err
	}
	r.forget()
	return nil
}

func (r *Registry) forget() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.until = time.Time{}
}

// All returns the node list, at most cacheFor old. If the database cannot be
// read, the last good list stays in use.
func (r *Registry) All() []model.Node {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := r.now(); now.After(r.until) {
		if nodes, err := r.store.LoadNodes(); err == nil {
			r.nodes = nodes
			r.until = now.Add(cacheFor)
		}
	}
	return r.nodes
}

// Edges returns the nodes that may cache encodes.
func (r *Registry) Edges() []model.Node {
	var out []model.Node
	for _, n := range r.All() {
		if n.Edge() {
			out = append(out, n)
		}
	}
	return out
}

// Edge finds an edge node by ID.
func (r *Registry) Edge(id string) *model.Node {
	for _, n := range r.Edges() {
		if n.ID == id {
			return &n
		}
	}
	return nil
}

// EdgeByName finds an edge node by name, as EDGE_VIEWERS gives it.
func (r *Registry) EdgeByName(name string) *model.Node {
	for _, n := range r.Edges() {
		if n.Name == name {
			return &n
		}
	}
	return nil
}

// ByID finds a live node by ID.
func (r *Registry) ByID(id string) *model.Node {
	for _, n := range r.All() {
		if n.ID == id {
			return &n
		}
	}
	return nil
}

// ByToken checks every node, so timing does not reveal which one matched.
func (r *Registry) ByToken(token string) *model.Node {
	hash := []byte(HashToken(token))
	var found *model.Node
	nodes := r.All()
	for i := range nodes {
		if subtle.ConstantTimeCompare(hash, []byte(nodes[i].TokenHash)) == 1 {
			found = &nodes[i]
		}
	}
	return found
}
