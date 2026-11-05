package sqlite

import (
	"fmt"
	"net/netip"
	"strings"

	"jellyfinner/internal/model"
)

// AddNode stores a new node.
func (s *Store) AddNode(n *model.Node) error {
	allow := make([]string, len(n.Allow))
	for i, prefix := range n.Allow {
		allow[i] = prefix.String()
	}
	res, err := s.db.Exec(`INSERT OR IGNORE INTO nodes (name, kind, token_hash, allow, url, quota_bytes, signing_key, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		n.Name, n.Kind, n.TokenHash, strings.Join(allow, ","), n.URL, n.QuotaBytes, n.SigningKey, s.now().Unix())
	if err != nil {
		return err
	}
	if added, err := res.RowsAffected(); err != nil {
		return err
	} else if added == 0 {
		return fmt.Errorf("%s: %w", n.Name, model.ErrNodeExists)
	}
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
		return fmt.Errorf("node %s: %w", name, model.ErrNotFound)
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
		return fmt.Errorf("node %s: %w", name, model.ErrNotFound)
	}
	return nil
}

// SetNodePaused pauses or resumes a node.
func (s *Store) SetNodePaused(name string, paused bool) error {
	res, err := s.db.Exec(`UPDATE nodes SET paused = ? WHERE name = ?`, paused, name)
	if err != nil {
		return err
	}
	if changed, err := res.RowsAffected(); err != nil {
		return err
	} else if changed == 0 {
		return fmt.Errorf("node %s: %w", name, model.ErrNotFound)
	}
	return nil
}

// LoadNodes reads every node from the database, by name.
func (s *Store) LoadNodes() ([]model.Node, error) {
	rows, err := s.db.Query(`SELECT name, kind, token_hash, allow, url, quota_bytes, signing_key, paused FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []model.Node
	for rows.Next() {
		var n model.Node
		var allow string
		if err := rows.Scan(&n.Name, &n.Kind, &n.TokenHash, &allow, &n.URL, &n.QuotaBytes, &n.SigningKey, &n.Paused); err != nil {
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
