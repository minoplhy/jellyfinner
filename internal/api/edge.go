package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"jellyfinner/internal/control"
	"jellyfinner/internal/edge"
	"jellyfinner/internal/model"
)

const (
	downloadTokenLife = time.Hour
	downloadDeadline  = 48 * time.Hour
)

// nodeID is the ID of the node that made the request. Nodes never send it:
// it comes from the token.
func nodeID(r *http.Request) string {
	node, _ := r.Context().Value(nodeIDKey).(string)
	return node
}

// nodeLabel is a node's name for logs, or its ID if it is gone.
func (s *Server) nodeLabel(id string) string {
	if n := s.nodes.ByID(id); n != nil {
		return n.Name
	}
	return id
}

// only keeps node roles apart: each call is for exactly one kind, so an edge
// node or router can never take encode jobs, and a worker can never reach
// edge orders, files or redirects.
func (s *Server) only(kind string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if n := s.nodes.ByID(nodeID(r)); n == nil || n.Kind != kind {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) edgeOnly(next http.HandlerFunc) http.HandlerFunc {
	return s.only(model.NodeEdge, next)
}

func (s *Server) workerOnly(next http.HandlerFunc) http.HandlerFunc {
	return s.only(model.NodeWorker, next)
}

func (s *Server) routerOnly(next http.HandlerFunc) http.HandlerFunc {
	return s.only(model.NodeRouter, next)
}

// routerResolve tells a router where a viewer should stream a library file
// from: a signed edge URL, or "" for the origin.
func (s *Server) routerResolve(w http.ResponseWriter, r *http.Request) {
	var req control.RouteRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Username == "" || req.Output == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var target string
	var err error
	if s.hooks.Route != nil {
		target, err = s.hooks.Route(req.Username, req.Output)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, control.RouteResponse{URL: target})
}

func (s *Server) edgePoll(w http.ResponseWriter, r *http.Request) {
	var req control.EdgePollRequest
	if !readJSON(w, r, &req) {
		return
	}
	node := nodeID(r)
	if err := s.store.NodeSeen(node, req.DiskFree, req.DiskTotal); err != nil {
		s.fail(w, err)
		return
	}
	placements, err := s.store.NodePlacements(node)
	if err != nil {
		s.fail(w, err)
		return
	}
	held := make(map[string]bool, len(req.Files))
	for _, hash := range req.Files {
		held[hash] = true
	}
	resp := control.EdgePollResponse{Orders: []control.EdgeOrder{}}
	known := make(map[string]bool, len(placements))
	for _, p := range placements {
		known[p.SHA256] = true
	}
	// A file nobody wants, such as one left by a delete that crossed a pull.
	for hash := range held {
		if !known[hash] && edge.ValidHash(hash) {
			resp.Orders = append(resp.Orders, control.EdgeOrder{Verb: control.VerbDelete, SHA256: hash})
		}
	}
	expires := s.Now().Add(downloadTokenLife)
	for _, p := range placements {
		switch p.State {
		case model.PlacementPulling:
			resp.Orders = append(resp.Orders, control.EdgeOrder{
				Verb: control.VerbPull, SHA256: p.SHA256, Size: p.Size,
				Token: edge.DownloadToken(s.cfg.EdgeTokenKey, node, p.SHA256, expires),
			})
		case model.PlacementDeleting:
			resp.Orders = append(resp.Orders, control.EdgeOrder{Verb: control.VerbDelete, SHA256: p.SHA256})
		case model.PlacementActive:
			if !held[p.SHA256] {
				// Never redirect viewers to a file the node says it lacks.
				if err := s.store.SetPlacementState(node, p.SHA256, model.PlacementFailed, "missing on node"); err != nil {
					s.fail(w, err)
					return
				}
				s.hooks.EdgeFailed(node, p.SHA256, "missing on node")
			}
		}
	}
	writeJSON(w, resp)
}

func (s *Server) edgeAck(w http.ResponseWriter, r *http.Request) {
	var req control.EdgeAck
	if !readJSON(w, r, &req) {
		return
	}
	if !edge.ValidHash(req.SHA256) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	node := nodeID(r)
	var err error
	switch {
	case req.Verb == control.VerbPull && req.Error == "":
		err = s.store.ActivatePlacement(node, req.SHA256)
	case req.Verb == control.VerbPull:
		var failed bool
		if failed, err = s.store.FailPlacement(node, req.SHA256, req.Error); failed {
			s.hooks.EdgeFailed(node, req.SHA256, req.Error)
		}
	case req.Verb == control.VerbDelete && req.Error == "":
		err = s.store.DeletePlacement(node, req.SHA256)
	case req.Verb == control.VerbDelete:
		s.log.Printf("node %s could not delete %s: %s", s.nodeLabel(node), req.SHA256, req.Error)
	default:
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// edgeFile serves one file, only to the node ordered to pull it, and only with
// a matching token. The path comes from the database, never the request.
func (s *Server) edgeFile(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	node := nodeID(r)
	if !edge.ValidHash(hash) {
		http.NotFound(w, r)
		return
	}
	if err := edge.CheckDownloadToken(s.cfg.EdgeTokenKey, node, hash, r.Header.Get("X-Download-Token"), s.Now()); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	p, err := s.store.PullingPlacement(node, hash)
	if errors.Is(err, model.ErrNotFound) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	f, err := os.Open(filepath.Join(s.cfg.LibraryDir, filepath.FromSlash(p.Output)))
	if err != nil {
		http.Error(w, "gone", http.StatusGone)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		s.fail(w, err)
		return
	}
	// The listener's write timeout suits small calls, not a capped pull.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(downloadDeadline)); err != nil {
		s.log.Printf("download deadline: %v", err)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", info.ModTime(), f)
}
