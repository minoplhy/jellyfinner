package queue

import (
	"database/sql"
	"errors"
)

// Placement states. Only an active placement is ever served.
const (
	PlacementPulling  = "pulling"
	PlacementActive   = "active"
	PlacementDeleting = "deleting"
	PlacementFailed   = "failed"
)

// Placement is one encoded file on one edge node. Files are identified by
// content hash, so a re-encode can never be served from a stale copy.
type Placement struct {
	Node       string
	SHA256     string
	Output     string
	Size       int64
	State      string
	Error      string
	LastPlayed int64
}

const placementColumns = `node, sha256, output, size, state, error, last_played`

func scanPlacement(row interface{ Scan(...any) error }) (*Placement, error) {
	var p Placement
	if err := row.Scan(&p.Node, &p.SHA256, &p.Output, &p.Size, &p.State, &p.Error, &p.LastPlayed); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) placements(q string, args ...any) ([]*Placement, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Placement
	for rows.Next() {
		p, err := scanPlacement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AddPlacement records a wanted file. A failed row for the same file is
// replaced, so an operator can simply try again.
func (s *Store) AddPlacement(p *Placement) error {
	_, err := s.db.Exec(`INSERT INTO placements (node, sha256, output, size, state) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (node, sha256) DO UPDATE SET state = excluded.state, error = ''
		WHERE placements.state = 'failed'`,
		p.Node, p.SHA256, p.Output, p.Size, PlacementPulling)
	return err
}

// Placements lists every placement, for the UI.
func (s *Store) Placements() ([]*Placement, error) {
	return s.placements(`SELECT ` + placementColumns + ` FROM placements ORDER BY node, output`)
}

// NodePlacements lists one node's placements.
func (s *Store) NodePlacements(node string) ([]*Placement, error) {
	return s.placements(`SELECT `+placementColumns+` FROM placements WHERE node = ?`, node)
}

// OutputPlacements lists the placements of one library path.
func (s *Store) OutputPlacements(output string) ([]*Placement, error) {
	return s.placements(`SELECT `+placementColumns+` FROM placements WHERE output = ?`, output)
}

// ActivePlacement returns the servable copy of a title on a node.
func (s *Store) ActivePlacement(output, node string) (*Placement, error) {
	p, err := scanPlacement(s.db.QueryRow(`SELECT `+placementColumns+` FROM placements
		WHERE output = ? AND node = ? AND state = ?`, output, node, PlacementActive))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// PullingPlacement returns a file a node has been ordered to pull.
func (s *Store) PullingPlacement(node, sha256 string) (*Placement, error) {
	p, err := scanPlacement(s.db.QueryRow(`SELECT `+placementColumns+` FROM placements
		WHERE node = ? AND sha256 = ? AND state = ?`, node, sha256, PlacementPulling))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// SetPlacementState moves a placement to a new state.
func (s *Store) SetPlacementState(node, sha256, state, message string) error {
	_, err := s.db.Exec(`UPDATE placements SET state = ?, error = ? WHERE node = ? AND sha256 = ?`,
		state, message, node, sha256)
	return err
}

// ActivatePlacement makes a pulled file servable, unless it was withdrawn
// while the node was pulling it.
func (s *Store) ActivatePlacement(node, sha256 string) error {
	_, err := s.db.Exec(`UPDATE placements SET state = ?, error = '' WHERE node = ? AND sha256 = ? AND state = ?`,
		PlacementActive, node, sha256, PlacementPulling)
	return err
}

// DeletePlacement forgets a placement.
func (s *Store) DeletePlacement(node, sha256 string) error {
	_, err := s.db.Exec(`DELETE FROM placements WHERE node = ? AND sha256 = ?`, node, sha256)
	return err
}

// TouchPlacement records that a viewer was sent to this copy.
func (s *Store) TouchPlacement(node, sha256 string) error {
	_, err := s.db.Exec(`UPDATE placements SET last_played = ? WHERE node = ? AND sha256 = ?`,
		s.Now().Unix(), node, sha256)
	return err
}

// RenamePlacements follows a title to a new library path.
func (s *Store) RenamePlacements(from, to string) error {
	_, err := s.db.Exec(`UPDATE placements SET output = ? WHERE output = ?`, to, from)
	return err
}

// NodeUsed returns the bytes a node holds or is pulling.
func (s *Store) NodeUsed(node string) (int64, error) {
	var used sql.NullInt64
	err := s.db.QueryRow(`SELECT SUM(size) FROM placements WHERE node = ? AND state IN ('pulling', 'active')`, node).Scan(&used)
	return used.Int64, err
}

// NodeStatus is what the orchestrator last learned about a node.
type NodeStatus struct {
	Node      string
	LastSeen  int64
	Healthy   bool
	DiskFree  int64
	DiskTotal int64
}

// NodeSeen records an agent's report.
func (s *Store) NodeSeen(node string, free, total int64) error {
	_, err := s.db.Exec(`INSERT INTO edge_status (node, last_seen, disk_free, disk_total) VALUES (?, ?, ?, ?)
		ON CONFLICT (node) DO UPDATE SET last_seen = excluded.last_seen,
			disk_free = excluded.disk_free, disk_total = excluded.disk_total`,
		node, s.Now().Unix(), free, total)
	return err
}

// SetNodeHealth records the result of a probe from B.
func (s *Store) SetNodeHealth(node string, healthy bool) error {
	_, err := s.db.Exec(`INSERT INTO edge_status (node, healthy) VALUES (?, ?)
		ON CONFLICT (node) DO UPDATE SET healthy = excluded.healthy`, node, healthy)
	return err
}

// GetNodeStatus returns a node's status, zero if it has never been heard from.
func (s *Store) GetNodeStatus(node string) (NodeStatus, error) {
	st := NodeStatus{Node: node}
	err := s.db.QueryRow(`SELECT last_seen, healthy, disk_free, disk_total FROM edge_status WHERE node = ?`, node).
		Scan(&st.LastSeen, &st.Healthy, &st.DiskFree, &st.DiskTotal)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	return st, err
}
