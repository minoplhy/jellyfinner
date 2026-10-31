package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"jellyfinner/internal/config"
	"jellyfinner/internal/edgerouter"
	"jellyfinner/internal/pipeline"
	"jellyfinner/internal/queue"
)

const (
	probeInterval = 30 * time.Second
	// recentPlay is how long after a redirect a title counts as being watched.
	recentPlay = 24 * time.Hour
)

// Pin orders a node to cache a finished encode.
func (s *Service) Pin(sourcePath, node string) error {
	n := s.Cfg.EdgeNode(node)
	if n == nil {
		return fmt.Errorf("unknown edge node %q", node)
	}
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
		if p.Node == node && p.SHA256 == src.EncodedSHA256 && p.State != queue.PlacementFailed {
			return fmt.Errorf("%s is already %s on %s", sourcePath, p.State, node)
		}
	}
	if err := s.checkQuota(n, src.EncodedBytes); err != nil {
		return err
	}
	return s.Store.AddPlacement(&queue.Placement{Node: node, SHA256: src.EncodedSHA256, Output: output, Size: src.EncodedBytes})
}

func (s *Service) checkQuota(n *config.EdgeNode, size int64) error {
	used, err := s.Store.NodeUsed(n.Name)
	if err != nil {
		return err
	}
	if used+size > n.QuotaBytes {
		return fmt.Errorf("%s is full: %d bytes used of %d, %d more needed", n.Name, used, n.QuotaBytes, size)
	}
	return nil
}

// Unpin removes a title from a node. Viewers already watching it would fail on
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
		if p.Node != node || p.State == queue.PlacementDeleting {
			continue
		}
		found = true
		if !force && p.LastPlayed != 0 && s.Store.Now().Unix()-p.LastPlayed < int64(recentPlay.Seconds()) {
			return fmt.Errorf("%s was played on %s recently, and a viewer's next seek would fail: use force to remove it", output, node)
		}
		if err := s.Store.SetPlacementState(node, p.SHA256, queue.PlacementDeleting, ""); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("%s is not on %s", output, node)
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
		if err := s.Store.SetPlacementState(p.Node, p.SHA256, queue.PlacementDeleting, ""); err != nil {
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
		if p.State != queue.PlacementDeleting {
			nodes[p.Node] = true
		}
		if err := s.Store.SetPlacementState(p.Node, p.SHA256, queue.PlacementDeleting, ""); err != nil {
			return err
		}
	}
	var errs []error
	for node := range nodes {
		n := s.Cfg.EdgeNode(node)
		if n == nil {
			continue
		}
		if err := s.checkQuota(n, video.Size); err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, s.Store.AddPlacement(&queue.Placement{Node: node, SHA256: video.SHA256, Output: output, Size: video.Size}))
	}
	return errors.Join(errs...)
}

func (s *Service) probeNodes(ctx context.Context) {
	client := &http.Client{Timeout: 10 * time.Second}
	for _, n := range s.Cfg.EdgeNodes {
		healthy := probe(ctx, client, n.URL+"/health")
		if err := s.Store.SetNodeHealth(n.Name, healthy); err != nil {
			s.Log.Printf("node health %s: %v", n.Name, err)
		}
	}
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

// NodeRow is one node in the UI.
type NodeRow struct {
	Name       string
	URL        string
	Quota      int64
	Used       int64
	Status     queue.NodeStatus
	Online     bool
	Placements []*queue.Placement
	Viewers    []string
}

// NodeRows summarises every edge node.
func (s *Service) NodeRows() ([]NodeRow, error) {
	var rows []NodeRow
	for _, n := range s.Cfg.EdgeNodes {
		row := NodeRow{Name: n.Name, URL: n.URL, Quota: n.QuotaBytes}
		var err error
		if row.Used, err = s.Store.NodeUsed(n.Name); err != nil {
			return nil, err
		}
		if row.Status, err = s.Store.GetNodeStatus(n.Name); err != nil {
			return nil, err
		}
		if row.Placements, err = s.Store.NodePlacements(n.Name); err != nil {
			return nil, err
		}
		row.Online = edgerouter.Servable(row.Status, s.Store.Now())
		for user, node := range s.Cfg.EdgeViewers {
			if node == n.Name {
				row.Viewers = append(row.Viewers, user)
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}
