package orchestrator

import (
	"errors"

	"jellyfinner/internal/edge"
	"jellyfinner/internal/model"
)

// EdgeRedirect answers a router: where should this Jellyfin user stream this
// library file from? It returns a signed URL on the user's edge node, or ""
// for the origin: when the user has no edge, the title is not active there,
// or the edge is not healthy. A redirect counts as a play of that copy.
//
// The router knows the username and the file from Jellyfin. Everything else,
// the signing keys included, stays here.
func (s *Service) EdgeRedirect(username, output string) (string, error) {
	node := s.Nodes.ViewerEdge(username)
	if node == nil || output == "" {
		return "", nil
	}
	p, err := s.Store.ActivePlacement(output, node.ID)
	if errors.Is(err, model.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	status, err := s.Store.GetNodeStatus(node.ID)
	if err != nil {
		return "", err
	}
	now := s.Now()
	if !edge.Servable(status, now) {
		return "", nil
	}
	if err := s.Store.TouchPlacement(node.ID, p.SHA256); err != nil {
		s.Log.Printf("router, record play: %v", err)
	}
	return edge.SignURL(node.URL, node.SigningKey, p.SHA256, now.Add(s.Cfg.EdgeURLTTL)), nil
}
