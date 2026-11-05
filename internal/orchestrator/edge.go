package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"jellyfinner/internal/edgerouter"
	"jellyfinner/internal/model"
	"jellyfinner/internal/pipeline"
)

const (
	probeInterval = 30 * time.Second
	// recentPlay is how long after a redirect a title counts as being watched.
	recentPlay = 24 * time.Hour
)

// Pin orders a node, by ID, to cache a finished encode.
func (s *Service) Pin(sourcePath, node string) error {
	n := s.Nodes.Edge(node)
	if n == nil {
		return fmt.Errorf("unknown edge node %q", node)
	}
	name := n.Name
	src, err := s.Store.Source(sourcePath)
	if err != nil {
		return fmt.Errorf("%s: %w", sourcePath, err)
	}
	// A stub has no recorded hash, so this refuses stubs and unfinished titles.
	if !src.Encoded() || src.EncodedSHA256 == "" {
		return fmt.Errorf("%s has no finished encode", sourcePath)
	}
	output := OutputPath(sourcePath)
	existing, err := s.Store.OutputPlacements(output)
	if err != nil {
		return err
	}
	for _, p := range existing {
		if p.Node == node && p.SHA256 == src.EncodedSHA256 && p.State != model.PlacementFailed {
			return fmt.Errorf("%s is already %s on %s", sourcePath, p.State, name)
		}
	}
	if err := s.checkQuota(n, src.EncodedBytes); err != nil {
		return err
	}
	return s.Store.AddPlacement(&model.Placement{Node: node, SHA256: src.EncodedSHA256, Output: output, Size: src.EncodedBytes})
}

func (s *Service) checkQuota(n *model.Node, size int64) error {
	used, err := s.Store.NodeUsed(n.ID)
	if err != nil {
		return err
	}
	if used+size > n.QuotaBytes {
		return fmt.Errorf("%s is full: %d bytes used of %d, %d more needed", n.Name, used, n.QuotaBytes, size)
	}
	return nil
}

// Unpin removes a title from a node, by ID. Viewers already watching it would fail on
// their next seek, so a recently played title needs force.
func (s *Service) Unpin(sourcePath, node string, force bool) error {
	return s.UnpinOutput(OutputPath(sourcePath), node, force)
}

// UnpinOutput is Unpin by library path, which is what the node page has.
func (s *Service) UnpinOutput(output, node string, force bool) error {
	placements, err := s.Store.OutputPlacements(output)
	if err != nil {
		return err
	}
	found := false
	for _, p := range placements {
		if p.Node != node || p.State == model.PlacementDeleting {
			continue
		}
		found = true
		if !force && p.LastPlayed != 0 && s.Now().Unix()-p.LastPlayed < int64(recentPlay.Seconds()) {
			return fmt.Errorf("%s was played on %s recently, and a viewer's next seek would fail: use force to remove it", output, s.NodeName(node))
		}
		if err := s.Store.SetPlacementState(node, p.SHA256, model.PlacementDeleting, ""); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("%s is not on %s", output, s.NodeName(node))
	}
	return nil
}

// withdrawPlacements stops serving a title and clears it from every node.
func (s *Service) withdrawPlacements(output string) error {
	placements, err := s.Store.OutputPlacements(output)
	if err != nil {
		return err
	}
	for _, p := range placements {
		if err := s.Store.SetPlacementState(p.Node, p.SHA256, model.PlacementDeleting, ""); err != nil {
			return err
		}
	}
	return nil
}

// refreshPlacements runs after a publish. Old copies stop being served at once,
// so viewers fall back to the origin, and the same nodes fetch the new file.
func (s *Service) refreshPlacements(output string, video pipeline.File) error {
	placements, err := s.Store.OutputPlacements(output)
	if err != nil {
		return err
	}
	nodes := map[string]bool{}
	for _, p := range placements {
		if p.SHA256 == video.SHA256 {
			continue
		}
		if p.State != model.PlacementDeleting {
			nodes[p.Node] = true
		}
		if err := s.Store.SetPlacementState(p.Node, p.SHA256, model.PlacementDeleting, ""); err != nil {
			return err
		}
	}
	var errs []error
	for node := range nodes {
		n := s.Nodes.Edge(node)
		if n == nil {
			continue
		}
		if err := s.checkQuota(n, video.Size); err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, s.Store.AddPlacement(&model.Placement{Node: node, SHA256: video.SHA256, Output: output, Size: video.Size}))
	}
	return errors.Join(errs...)
}

func (s *Service) probeNodes(ctx context.Context) {
	client := &http.Client{Timeout: 10 * time.Second}
	for _, n := range s.Nodes.Edges() {
		healthy := probe(ctx, client, strings.TrimSuffix(n.URL, "/")+"/health")
		if err := s.Store.SetNodeHealth(n.ID, healthy); err != nil {
			s.Log.Printf("node health %s: %v", n.Name, err)
		}
	}
}

// LegacyViewersNote is said wherever EDGE_VIEWERS is still set.
const LegacyViewersNote = "EDGE_VIEWERS is set but no longer read, so it sends nobody to an edge node. " +
	"Assign viewers on each edge node's Edit page, or with orchestrator edge assign, then remove the variable"

// EdgeViewers lists the users sent to one edge node, by ID.
func (s *Service) EdgeViewers(edge string) []string {
	var out []string
	for _, v := range s.Nodes.Viewers() {
		if v.Node == edge {
			out = append(out, v.Username)
		}
	}
	return out
}

// AssignViewer sends a Jellyfin user to an edge node, by ID, within seconds.
// It returns the name of the edge the user moved from, if any.
func (s *Service) AssignViewer(username, edge string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", errors.New("a viewer needs a Jellyfin username")
	}
	n := s.Nodes.Edge(edge)
	if n == nil {
		if _, err := s.NodeByID(edge); err == nil {
			return "", errors.New("viewers can only be sent to an edge node")
		}
		return "", fmt.Errorf("edge node %s: %w", edge, model.ErrNotFound)
	}
	var from string
	for _, v := range s.Nodes.Viewers() {
		if strings.EqualFold(v.Username, username) && v.Node != edge {
			from = s.NodeName(v.Node)
		}
	}
	if err := s.Nodes.SetViewer(username, edge); err != nil {
		return "", err
	}
	return from, nil
}

// UnassignViewer sends a user back to the origin.
func (s *Service) UnassignViewer(username string) error {
	return s.Nodes.RemoveViewer(strings.TrimSpace(username))
}

// AllowString writes allowed addresses as node add and node edit take them.
func AllowString(prefixes []netip.Prefix) string {
	out := make([]string, len(prefixes))
	for i, p := range prefixes {
		out[i] = p.String()
	}
	return strings.Join(out, ", ")
}

func probe(ctx context.Context, client *http.Client, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// EdgeCopyFailed tells the operator, because nothing retries on its own: a node
// that keeps failing must not keep pulling. node is an ID.
func (s *Service) EdgeCopyFailed(ctx context.Context, node, sha256, reason string) {
	s.send(ctx, fmt.Sprintf("Edge copy failed on %s: %s (file %.12s). Viewers stay on the origin until it is pinned again.", s.NodeName(node), reason, sha256))
}

// NodeRow is one node in the UI.
type NodeRow struct {
	ID         string
	Name       string
	Allow      string // comma-separated CIDR, as typed
	URL        string
	Quota      int64
	Used       int64
	Status     model.NodeStatus
	Online     bool
	Placements []*model.Placement
	Viewers    []string
}

// NodeRows summarises every edge node.
func (s *Service) NodeRows() ([]NodeRow, error) {
	var rows []NodeRow
	for _, n := range s.Nodes.Edges() {
		row := NodeRow{ID: n.ID, Name: n.Name, Allow: AllowString(n.Allow), URL: n.URL, Quota: n.QuotaBytes}
		var err error
		if row.Used, err = s.Store.NodeUsed(n.ID); err != nil {
			return nil, err
		}
		if row.Status, err = s.Store.GetNodeStatus(n.ID); err != nil {
			return nil, err
		}
		if row.Placements, err = s.Store.NodePlacements(n.ID); err != nil {
			return nil, err
		}
		row.Online = edgerouter.Servable(row.Status, s.Now())
		row.Viewers = s.EdgeViewers(n.ID)
		rows = append(rows, row)
	}
	return rows, nil
}
