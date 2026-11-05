package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"jellyfinner/internal/model"
)

func joinAllow(prefixes []netip.Prefix) string {
	allow := make([]string, len(prefixes))
	for i, prefix := range prefixes {
		allow[i] = prefix.String()
	}
	return strings.Join(allow, ",")
}

// AddNode stores a new node. Its name must be free among live nodes; a removed
// node's name may be taken again.
func (s *Store) AddNode(n *model.Node) error {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO nodes (id, name, kind, token_hash, allow, url, quota_bytes, signing_key, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.Name, n.Kind, n.TokenHash, joinAllow(n.Allow), n.URL, n.QuotaBytes, n.SigningKey, s.now().Unix())
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

// UpdateNode saves a live node's name, allowed addresses, URL and quota.
func (s *Store) UpdateNode(n *model.Node) error {
	return s.tx(func(tx *sql.Tx) error {
		var clash string
		err := tx.QueryRow(`SELECT id FROM nodes WHERE name = ? AND removed = 0 AND id != ?`, n.Name, n.ID).Scan(&clash)
		if err == nil {
			return fmt.Errorf("%s: %w", n.Name, model.ErrNodeExists)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		res, err := tx.Exec(`UPDATE nodes SET name = ?, allow = ?, url = ?, quota_bytes = ? WHERE id = ? AND removed = 0`,
			n.Name, joinAllow(n.Allow), n.URL, n.QuotaBytes, n.ID)
		if err != nil {
			return err
		}
		return requireChange(res, n.ID)
	})
}

// RemoveNode revokes a node and forgets what it cached. The files stay on the
// node, because nothing can order it to delete them any more. The row stays,
// without its secrets, so history keeps the node's name.
func (s *Store) RemoveNode(id string) error {
	return s.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE nodes SET removed = ?, token_hash = '', signing_key = '', paused = 0
			WHERE id = ? AND removed = 0`, s.now().Unix(), id)
		if err != nil {
			return err
		}
		if err := requireChange(res, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM placements WHERE node = ?`, id); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM edge_status WHERE node = ?`, id)
		return err
	})
}

// SetNodeToken replaces a node's token, which revokes the old one.
func (s *Store) SetNodeToken(id, tokenHash string) error {
	res, err := s.db.Exec(`UPDATE nodes SET token_hash = ? WHERE id = ? AND removed = 0`, tokenHash, id)
	if err != nil {
		return err
	}
	return requireChange(res, id)
}

// SetNodePaused pauses or resumes a node.
func (s *Store) SetNodePaused(id string, paused bool) error {
	res, err := s.db.Exec(`UPDATE nodes SET paused = ? WHERE id = ? AND removed = 0`, paused, id)
	if err != nil {
		return err
	}
	return requireChange(res, id)
}

func requireChange(res sql.Result, id string) error {
	if changed, err := res.RowsAffected(); err != nil {
		return err
	} else if changed == 0 {
		return fmt.Errorf("node %s: %w", id, model.ErrNotFound)
	}
	return nil
}

// LoadNodes reads every live node, by name.
func (s *Store) LoadNodes() ([]model.Node, error) {
	rows, err := s.db.Query(`SELECT id, name, kind, token_hash, allow, url, quota_bytes, signing_key, paused
		FROM nodes WHERE removed = 0 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []model.Node
	for rows.Next() {
		var n model.Node
		var allow string
		if err := rows.Scan(&n.ID, &n.Name, &n.Kind, &n.TokenHash, &allow, &n.URL, &n.QuotaBytes, &n.SigningKey, &n.Paused); err != nil {
			return nil, err
		}
		if n.Allow, err = parseAllow(allow); err != nil {
			return nil, fmt.Errorf("node %s: %w", n.Name, err)
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// NodeNames maps every node ID to its name, removed nodes included.
func (s *Store) NodeNames() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT id, name FROM nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
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
