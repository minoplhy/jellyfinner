package sqlite

import (
	"fmt"

	"jellyfinner/internal/model"
)

// SetViewer sends a user to an edge node, replacing any earlier assignment.
// The stored spelling is the latest one given.
func (s *Store) SetViewer(username, node string) error {
	_, err := s.db.Exec(`INSERT INTO edge_viewers (username, node) VALUES (?, ?)
		ON CONFLICT (username) DO UPDATE SET username = excluded.username, node = excluded.node`, username, node)
	return err
}

// RemoveViewer sends a user back to the origin.
func (s *Store) RemoveViewer(username string) error {
	res, err := s.db.Exec(`DELETE FROM edge_viewers WHERE username = ?`, username)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("viewer %s: %w", username, model.ErrNotFound)
	}
	return nil
}

// Viewers lists every assignment, by username.
func (s *Store) Viewers() ([]model.Viewer, error) {
	rows, err := s.db.Query(`SELECT username, node FROM edge_viewers ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Viewer
	for rows.Next() {
		var v model.Viewer
		if err := rows.Scan(&v.Username, &v.Node); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
