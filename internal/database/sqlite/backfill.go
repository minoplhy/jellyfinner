package sqlite

import (
	"database/sql"
	"errors"
	"strconv"

	"jellyfinner/internal/model"
)

// spendBudget decides whether a backfill job may start now. A pause stops
// queued jobs too, not just new ones. Bytes are charged at claim time, so a
// retry pays again, as it moves the data again. A title larger than the whole
// budget may run on an unused day, or it would block the backfill forever.
func (s *Store) spendBudget(tx *sql.Tx, size int64) (bool, error) {
	var active string
	err := tx.QueryRow(`SELECT value FROM meta WHERE key = ?`, model.MetaBackfillActive).Scan(&active)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if active != "1" {
		return false, nil
	}
	if s.budget <= 0 {
		return true, nil
	}
	day := s.now().UTC().Format("2006-01-02")
	used, err := budgetUsed(tx, day)
	if err != nil {
		return false, err
	}
	if used > 0 && used+size > s.budget {
		return false, nil
	}
	_, err = tx.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		model.MetaBackfillUsedPrefix+day, strconv.FormatInt(used+size, 10))
	return true, err
}

type querier interface {
	QueryRow(string, ...any) *sql.Row
}

func budgetUsed(q querier, day string) (int64, error) {
	var value string
	err := q.QueryRow(`SELECT value FROM meta WHERE key = ?`, model.MetaBackfillUsedPrefix+day).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(value, 10, 64)
}

// BudgetUsedToday returns the source bytes backfill has started today.
func (s *Store) BudgetUsedToday() (int64, error) {
	return budgetUsed(s.db, s.now().UTC().Format("2006-01-02"))
}

// JobsByID returns every job's state, for working out what backfill has left.
func (s *Store) JobsByID() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT id, state FROM jobs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			return nil, err
		}
		out[id] = state
	}
	return out, rows.Err()
}

// LatestJobs returns the newest job for each source path.
func (s *Store) LatestJobs() (map[string]*model.Job, error) {
	jobs, err := s.query(`SELECT ` + jobColumns + ` FROM jobs ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*model.Job, len(jobs))
	for _, j := range jobs {
		out[j.Path] = j
	}
	return out, nil
}

// CancelStaleProfile cancels queued jobs built for an older output profile,
// which would otherwise run with the new settings under the old job ID.
func (s *Store) CancelStaleProfile(current string) (int64, error) {
	res, err := s.db.Exec(`UPDATE jobs SET state = ?, updated = ? WHERE state = ? AND profile != ?`,
		model.StateCancelled, s.now().Unix(), model.StateQueued, current)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// StateCounts returns the number of jobs in each state.
func (s *Store) StateCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT state, COUNT(*) FROM jobs GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}
