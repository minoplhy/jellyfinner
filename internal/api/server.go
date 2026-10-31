// Package api is the control API that workers call. Nodes always connect out
// to it, on a listener separate from anything public-facing.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"jellyfinner/internal/config"
	"jellyfinner/internal/control"
	"jellyfinner/internal/job"
	"jellyfinner/internal/queue"
)

const (
	maxBody     = 1 << 20
	authLimit   = 10
	authWindow  = time.Minute
	nodeNameKey = contextKey("node")
)

type contextKey string

// Hooks connect the API to the rest of the orchestrator.
type Hooks struct {
	WorkerJob func(*queue.Job) *job.Job
	Completed func()
	Failed    func(*queue.Job)
}

// Server handles control requests.
type Server struct {
	cfg   *config.Config
	store *queue.Store
	hooks Hooks
	log   *log.Logger

	mu       sync.Mutex
	failures map[netip.Addr]*failure
	seen     map[string]time.Time
}

type failure struct {
	count int
	since time.Time
}

// New returns a Server.
func New(cfg *config.Config, store *queue.Store, hooks Hooks, logger *log.Logger) *Server {
	return &Server{cfg: cfg, store: store, hooks: hooks, log: logger, failures: map[netip.Addr]*failure{}, seen: map[string]time.Time{}}
}

// Handler returns the routes behind authentication.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/whoami", s.whoami)
	mux.HandleFunc("POST /v1/jobs/claim", s.workerOnly(s.claim))
	mux.HandleFunc("POST /v1/jobs/{id}/heartbeat", s.workerOnly(s.heartbeat))
	mux.HandleFunc("POST /v1/jobs/{id}/finish", s.workerOnly(s.finish))
	mux.HandleFunc("GET /v1/jobs/{id}/source", s.workerOnly(s.jobSource))
	mux.HandleFunc("HEAD /v1/jobs/{id}/files/{name}", s.workerOnly(s.jobFileOffset))
	mux.HandleFunc("PUT /v1/jobs/{id}/files/{name}", s.workerOnly(s.jobFileUpload))
	mux.HandleFunc("POST /v1/edge/poll", s.edgeOnly(s.edgePoll))
	mux.HandleFunc("POST /v1/edge/ack", s.edgeOnly(s.edgeAck))
	mux.HandleFunc("GET /v1/edge/files/{hash}", s.edgeOnly(s.edgeFile))
	return s.authenticate(mux)
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr, ok := s.cfg.ClientAddr(r)
		if !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if s.limited(addr) {
			http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
			return
		}
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		node := s.store.NodeByToken(token)
		if node == nil {
			s.recordFailure(addr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !allowed(node, addr) {
			s.recordFailure(addr)
			s.log.Printf("node %s refused from %s", node.Name, addr)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		s.markSeen(node.Name)
		// Uploads carry a whole file and set their own limit.
		if r.Method != http.MethodPut {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		ctx := context.WithValue(r.Context(), nodeNameKey, node.Name)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// whoami lets a node test its token without changing anything, because claiming
// a job to find out could take real work.
func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	kind := queue.NodeWorker
	if s.store.EdgeNode(nodeName(r)) != nil {
		kind = queue.NodeEdge
	}
	writeJSON(w, control.WhoAmIResponse{Name: nodeName(r), Kind: kind})
}

func (s *Server) markSeen(node string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen[node] = s.store.Now()
}

// LastSeen returns when each node last made an authenticated call.
func (s *Server) LastSeen() map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]time.Time, len(s.seen))
	for node, at := range s.seen {
		out[node] = at
	}
	return out
}

func allowed(n *queue.Node, addr netip.Addr) bool {
	if len(n.Allow) == 0 {
		return true
	}
	for _, prefix := range n.Allow {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func (s *Server) limited(addr netip.Addr) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.failures[addr]
	if f == nil {
		return false
	}
	if s.store.Now().Sub(f.since) > authWindow {
		delete(s.failures, addr)
		return false
	}
	return f.count >= authLimit
}

func (s *Server) recordFailure(addr netip.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.failures[addr]
	if f == nil {
		s.failures[addr] = &failure{count: 1, since: s.store.Now()}
		return
	}
	f.count++
}

func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	node, _ := r.Context().Value(nodeNameKey).(string)
	j, err := s.store.Claim(node, s.cfg.LeaseFor)
	if err != nil {
		s.fail(w, err)
		return
	}
	if j == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, control.ClaimResponse{Job: *s.hooks.WorkerJob(j), Lease: j.LeaseID})
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	var req control.HeartbeatRequest
	if !readJSON(w, r, &req) {
		return
	}
	cancel, err := s.store.Heartbeat(r.PathValue("id"), req.Lease, req.Stage, req.Fraction, s.cfg.LeaseFor)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, control.HeartbeatResponse{Cancel: cancel})
}

func (s *Server) finish(w http.ResponseWriter, r *http.Request) {
	var req control.FinishRequest
	if !readJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if req.Error == "" {
		if len(req.Manifest) == 0 {
			http.Error(w, "manifest required", http.StatusBadRequest)
			return
		}
		if err := s.store.Complete(id, req.Lease, string(req.Manifest), req.Log); err != nil {
			s.fail(w, err)
			return
		}
		s.hooks.Completed()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	final, err := s.store.Fail(id, req.Lease, req.Error, req.Log, s.cfg.MaxAttempts)
	if err != nil {
		s.fail(w, err)
		return
	}
	if final {
		if j, err := s.store.Get(id); err == nil {
			s.hooks.Failed(j)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, queue.ErrLeaseLost) {
		http.Error(w, "lease lost", http.StatusConflict)
		return
	}
	s.log.Printf("control api: %v", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func readJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
