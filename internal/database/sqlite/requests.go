package sqlite

import (
	"database/sql"
	"fmt"

	"jellyfinner/internal/model"
)

// AddRequest records a viewer's request once per title.
func (s *Store) AddRequest(r model.Request) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO requests (path, user_id, username, item_id) VALUES (?, ?, ?, ?)`,
		r.Path, r.UserID, r.Username, r.ItemID)
	return err
}

// TakeRequests returns a title's requests and removes them.
func (s *Store) TakeRequests(path string) ([]model.Request, error) {
	rows, err := s.db.Query(`DELETE FROM requests WHERE path = ? RETURNING path, user_id, username, item_id`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Request
	for rows.Next() {
		var r model.Request
		if err := rows.Scan(&r.Path, &r.UserID, &r.Username, &r.ItemID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RequestedPaths returns every title with a request still open.
func (s *Store) RequestedPaths() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT DISTINCT path FROM requests`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		out[path] = true
	}
	return out, rows.Err()
}

// SwitchMode changes the active mode as one transaction. Going to auto moves
// unencoded titles to the backlog, so nothing starts without the backfill's
// traffic budget. Going to manual cancels queued work, and a running job
// finishes normally.
func (s *Store) SwitchMode(to string) (int64, error) {
	var affected int64
	err := s.tx(func(tx *sql.Tx) error {
		var res sql.Result
		var err error
		if to == "manual" {
			res, err = tx.Exec(`UPDATE jobs SET state = ?, updated = ? WHERE state = ?`,
				model.StateCancelled, s.now().Unix(), model.StateQueued)
			if err == nil {
				_, err = tx.Exec(`DELETE FROM meta WHERE key = ?`, model.MetaBackfillActive)
			}
		} else {
			res, err = tx.Exec(`UPDATE sources SET backlog = 1 WHERE encoded_size < 0`)
		}
		if err != nil {
			return fmt.Errorf("switch mode: %w", err)
		}
		if affected, err = res.RowsAffected(); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, model.MetaActiveMode, to)
		return err
	})
	return affected, err
}
