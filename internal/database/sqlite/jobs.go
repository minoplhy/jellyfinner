package sqlite

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"jellyfinner/internal/model"
)

const jobColumns = `id, path, output_path, source_size, source_mtime, revision, profile, priority,
	state, attempts, lease_id, node, stage, fraction, reason, force_encode, software,
	error, log, manifest, created, updated, started, finished`

func scanJob(row interface{ Scan(...any) error }) (*model.Job, error) {
	var j model.Job
	err := row.Scan(&j.ID, &j.Path, &j.OutputPath, &j.SourceSize, &j.SourceMtime, &j.Revision, &j.Profile, &j.Priority,
		&j.State, &j.Attempts, &j.LeaseID, &j.Node, &j.Stage, &j.Fraction, &j.Reason, &j.ForceEncode, &j.Software,
		&j.Error, &j.Log, &j.Manifest, &j.Created, &j.Updated, &j.Started, &j.Finished)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// Enqueue inserts a job. It reports false if the ID already exists, whatever
// its state, so the watcher cannot endlessly requeue a failed encode.
func (s *Store) Enqueue(j *model.Job) (bool, error) {
	now := s.now().Unix()
	res, err := s.db.Exec(`INSERT OR IGNORE INTO jobs
		(id, path, output_path, source_size, source_mtime, revision, profile, priority, state,
		 reason, force_encode, software, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.Path, j.OutputPath, j.SourceSize, j.SourceMtime, j.Revision, j.Profile, j.Priority, model.StateQueued,
		j.Reason, j.ForceEncode, j.Software, now, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Get returns one job.
func (s *Store) Get(id string) (*model.Job, error) {
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id))
	return j, noRow(err)
}

// List returns the most recent jobs, active ones first.
func (s *Store) List(limit int) ([]*model.Job, error) {
	return s.query(`SELECT `+jobColumns+` FROM jobs
		ORDER BY state IN ('running', 'publishing') DESC, state = 'queued' DESC, priority DESC, created DESC
		LIMIT ?`, limit)
}

// ListFiltered returns one page of matching jobs, ordered as List orders them,
// and how many match in all.
func (s *Store) ListFiltered(f model.JobFilter, limit, offset int) ([]*model.Job, int, error) {
	where := `(? = '' OR state = ?) AND (? = '' OR node = ?) AND (? = '' OR path LIKE ? ESCAPE '\')`
	like := "%" + likeEscaper.Replace(f.Search) + "%"
	args := []any{f.State, f.State, f.Node, f.Node, f.Search, like}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	jobs, err := s.query(`SELECT `+jobColumns+` FROM jobs WHERE `+where+`
		ORDER BY state IN ('running', 'publishing') DESC, state = 'queued' DESC, priority DESC, created DESC
		LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	return jobs, total, err
}

// likeEscaper makes a search term match literally inside LIKE.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// FailedByNode counts, per node, the jobs that failed for good since a time.
func (s *Store) FailedByNode(since time.Time) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT node, COUNT(*) FROM jobs WHERE state = ? AND node != '' AND updated >= ?
		GROUP BY node`, model.StateFailed, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var node string
		var n int
		if err := rows.Scan(&node, &n); err != nil {
			return nil, err
		}
		counts[node] = n
	}
	return counts, rows.Err()
}

// RunningByNode counts each node's running jobs.
func (s *Store) RunningByNode() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT node, COUNT(*) FROM jobs WHERE state = ? AND node != '' GROUP BY node`, model.StateRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var node string
		var n int
		if err := rows.Scan(&node, &n); err != nil {
			return nil, err
		}
		counts[node] = n
	}
	return counts, rows.Err()
}

// WorkByNode sums, per node, the jobs that reached done since a time.
func (s *Store) WorkByNode(since time.Time) (map[string]model.NodeWork, error) {
	rows, err := s.db.Query(`SELECT node, COUNT(*), COALESCE(SUM(source_size), 0) FROM jobs
		WHERE state = ? AND node != '' AND updated >= ? GROUP BY node`, model.StateDone, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	work := map[string]model.NodeWork{}
	for rows.Next() {
		var node string
		var w model.NodeWork
		if err := rows.Scan(&node, &w.Jobs, &w.SourceBytes); err != nil {
			return nil, err
		}
		work[node] = w
	}
	return work, rows.Err()
}

// Outcomes returns jobs that ended in [from, to): published, failed or
// cancelled. A published job's updated time is when it reached the library.
func (s *Store) Outcomes(from, to time.Time) ([]model.JobOutcome, error) {
	rows, err := s.db.Query(`SELECT node, state, source_size, attempts, started, finished, updated, manifest
		FROM jobs WHERE state IN (?, ?, ?) AND updated >= ? AND updated < ? ORDER BY updated`,
		model.StateDone, model.StateFailed, model.StateCancelled, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.JobOutcome
	for rows.Next() {
		var o model.JobOutcome
		if err := rows.Scan(&o.Node, &o.State, &o.SourceSize, &o.Attempts, &o.Started, &o.Finished, &o.Updated, &o.Manifest); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// FirstActivity returns when the first job was queued, or 0 with no jobs.
func (s *Store) FirstActivity() (int64, error) {
	var first sql.NullInt64
	err := s.db.QueryRow(`SELECT MIN(created) FROM jobs`).Scan(&first)
	return first.Int64, err
}

// Waiting describes the queued jobs a worker could claim now. Backfill counts
// only when asked for and running, because a paused backfill cannot be claimed.
func (s *Store) Waiting(includeBackfill bool) (model.QueueWait, error) {
	var w model.QueueWait
	backfill := false
	if includeBackfill {
		active, err := s.Meta(model.MetaBackfillActive)
		if err != nil {
			return w, err
		}
		backfill = active == "1"
	}
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(source_size), 0), COALESCE(MIN(created), 0) FROM jobs
		WHERE state = 'queued' AND not_before <= ? AND (priority != ? OR ?)`,
		s.now().Unix(), model.PriorityBackfill, backfill).Scan(&w.Count, &w.SourceBytes, &w.Oldest)
	return w, err
}

// Publishing returns jobs waiting to be published, including any left by a
// restart.
func (s *Store) Publishing() ([]*model.Job, error) {
	return s.query(`SELECT `+jobColumns+` FROM jobs WHERE state = ? ORDER BY updated`, model.StatePublishing)
}

func (s *Store) query(q string, args ...any) ([]*model.Job, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []*model.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// Claim leases the highest-priority queued job to a node, or returns nil.
func (s *Store) Claim(node string, leaseFor time.Duration) (*model.Job, error) {
	var claimed *model.Job
	err := s.tx(func(tx *sql.Tx) error {
		// Checked in the same transaction, so a pause holds from the moment it
		// is set, with no window for one more job. A name with no row, such
		// as in tests, is not paused.
		var paused bool
		err := tx.QueryRow(`SELECT paused FROM nodes WHERE name = ?`, node).Scan(&paused)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if paused {
			return nil
		}
		j, err := scanJob(tx.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE state = 'queued' AND not_before <= ?
			ORDER BY priority DESC, created, rowid LIMIT 1`, s.now().Unix()))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if j.Priority == model.PriorityBackfill {
			ok, err := s.spendBudget(tx, j.SourceSize)
			if err != nil || !ok {
				return err
			}
		}
		lease := make([]byte, 16)
		if _, err := rand.Read(lease); err != nil {
			return err
		}
		j.LeaseID = hex.EncodeToString(lease)
		j.Node = node
		j.Attempts++
		j.State = model.StateRunning
		now := s.now()
		_, err = tx.Exec(`UPDATE jobs SET state = ?, lease_id = ?, lease_expires = ?, node = ?,
			attempts = ?, stage = '', fraction = 0, error = '', updated = ?, started = ?, finished = 0 WHERE id = ?`,
			model.StateRunning, j.LeaseID, now.Add(leaseFor).Unix(), node, j.Attempts, now.Unix(), now.Unix(), j.ID)
		j.Started, j.Finished = now.Unix(), 0
		claimed = j
		return err
	})
	return claimed, err
}

// Heartbeat extends a lease and records progress. It reports true when the
// job was cancelled and the worker must stop.
func (s *Store) Heartbeat(id, lease, stage string, fraction float64, leaseFor time.Duration) (bool, error) {
	var cancel bool
	err := s.tx(func(tx *sql.Tx) error {
		var state, current string
		err := tx.QueryRow(`SELECT state, lease_id FROM jobs WHERE id = ?`, id).Scan(&state, &current)
		if errors.Is(err, sql.ErrNoRows) || current != lease {
			return model.ErrLeaseLost
		}
		if err != nil {
			return err
		}
		switch state {
		case model.StateCancelled:
			cancel = true
			return nil
		case model.StateRunning:
		default:
			return model.ErrLeaseLost
		}
		now := s.now()
		_, err = tx.Exec(`UPDATE jobs SET lease_expires = ?, stage = ?, fraction = ?, updated = ? WHERE id = ?`,
			now.Add(leaseFor).Unix(), stage, fraction, now.Unix(), id)
		return err
	})
	return cancel, err
}

// Complete moves a job to publishing. The worker's part is done, but nothing
// reaches the library until the orchestrator has checked the upload.
func (s *Store) Complete(id, lease, manifest, log string) error {
	res, err := s.db.Exec(`UPDATE jobs SET state = ?, manifest = ?, log = ?, stage = 'publish', fraction = 1,
		lease_id = '', updated = ?, finished = ? WHERE id = ? AND lease_id = ? AND lease_id != '' AND state = ?`,
		model.StatePublishing, manifest, log, s.now().Unix(), s.now().Unix(), id, lease, model.StateRunning)
	return leaseResult(res, err)
}

func leaseResult(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return model.ErrLeaseLost
	}
	return nil
}

// Release requeues a job its worker gave up to shut down. That says nothing
// about the job, so the attempt is given back.
func (s *Store) Release(id, lease string) error {
	res, err := s.db.Exec(`UPDATE jobs SET state = ?, attempts = attempts - 1, lease_id = '', stage = '', fraction = 0,
		updated = ? WHERE id = ? AND lease_id = ? AND lease_id != '' AND state = ?`,
		model.StateQueued, s.now().Unix(), id, lease, model.StateRunning)
	return leaseResult(res, err)
}

// Requeue takes a running job from its worker and queues it again, without
// spending an attempt. The worker's next heartbeat finds its lease gone, so it
// stops and drops its result. It returns the node the job came from.
func (s *Store) Requeue(id string) (string, error) {
	var node string
	err := s.tx(func(tx *sql.Tx) error {
		var state string
		err := tx.QueryRow(`SELECT state, node FROM jobs WHERE id = ?`, id).Scan(&state, &node)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("job %s: %w", id, model.ErrNotFound)
		}
		if err != nil {
			return err
		}
		if state != model.StateRunning {
			return fmt.Errorf("job %s is %s, not running", id, state)
		}
		_, err = tx.Exec(`UPDATE jobs SET state = ?, attempts = MAX(attempts - 1, 0), lease_id = '', stage = '', fraction = 0,
			not_before = 0, updated = ? WHERE id = ? AND state = ?`,
			model.StateQueued, s.now().Unix(), id, model.StateRunning)
		return err
	})
	return node, err
}

// Fail records a worker failure. The job is retried until maxAttempts, and
// true is returned once it has failed for good.
func (s *Store) Fail(id, lease, reason, log string, maxAttempts int) (bool, error) {
	var final bool
	err := s.tx(func(tx *sql.Tx) error {
		var state, current string
		var attempts int
		err := tx.QueryRow(`SELECT state, lease_id, attempts FROM jobs WHERE id = ?`, id).Scan(&state, &current, &attempts)
		if errors.Is(err, sql.ErrNoRows) || current != lease || lease == "" {
			return model.ErrLeaseLost
		}
		if err != nil {
			return err
		}
		if state == model.StateCancelled {
			_, err := tx.Exec(`UPDATE jobs SET log = ?, lease_id = '', updated = ? WHERE id = ?`, log, s.now().Unix(), id)
			return err
		}
		if state != model.StateRunning {
			return model.ErrLeaseLost
		}
		final = attempts >= maxAttempts
		_, err = tx.Exec(`UPDATE jobs SET state = ?, error = ?, log = ?, lease_id = '', not_before = ?, updated = ? WHERE id = ?`,
			nextState(final), reason, log, s.retryAt(attempts), s.now().Unix(), id)
		return err
	})
	return final, err
}

func (s *Store) retryAt(attempts int) int64 {
	return s.now().Add(time.Duration(attempts) * s.retryDelay).Unix()
}

func nextState(final bool) string {
	if final {
		return model.StateFailed
	}
	return model.StateQueued
}

// ExpireLeases requeues running jobs whose worker went silent, and returns
// those that have now failed for good.
func (s *Store) ExpireLeases(maxAttempts int) ([]*model.Job, error) {
	var failed []*model.Job
	err := s.tx(func(tx *sql.Tx) error {
		now := s.now().Unix()
		rows, err := tx.Query(`SELECT `+jobColumns+` FROM jobs WHERE state = ? AND lease_expires < ?`, model.StateRunning, now)
		if err != nil {
			return err
		}
		var expired []*model.Job
		for rows.Next() {
			j, err := scanJob(rows)
			if err != nil {
				rows.Close()
				return err
			}
			expired = append(expired, j)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, j := range expired {
			final := j.Attempts >= maxAttempts
			j.Error = "worker lease expired"
			if _, err := tx.Exec(`UPDATE jobs SET state = ?, error = ?, lease_id = '', not_before = ?, updated = ? WHERE id = ?`,
				nextState(final), j.Error, s.retryAt(j.Attempts), now, j.ID); err != nil {
				return err
			}
			if final {
				failed = append(failed, j)
			}
		}
		return nil
	})
	return failed, err
}

// MarkDone records a successful publish.
func (s *Store) MarkDone(id string) error {
	_, err := s.db.Exec(`UPDATE jobs SET state = ?, stage = 'done', error = '', updated = ? WHERE id = ?`,
		model.StateDone, s.now().Unix(), id)
	return err
}

// PublishFailed requeues a job whose upload the orchestrator rejected, and reports true once
// it has failed for good.
func (s *Store) PublishFailed(id, reason string, maxAttempts int) (bool, error) {
	j, err := s.Get(id)
	if err != nil {
		return false, err
	}
	final := j.Attempts >= maxAttempts
	_, err = s.db.Exec(`UPDATE jobs SET state = ?, error = ?, not_before = ?, updated = ? WHERE id = ?`,
		nextState(final), reason, s.retryAt(j.Attempts), s.now().Unix(), id)
	return final, err
}

// Cancel stops a queued or running job. A running worker learns of it on its
// next heartbeat.
func (s *Store) Cancel(id string) error {
	res, err := s.db.Exec(`UPDATE jobs SET state = ?, updated = ? WHERE id = ? AND state IN ('queued', 'running')`,
		model.StateCancelled, s.now().Unix(), id)
	if err != nil {
		return err
	}
	return requireRow(res, id, "is not queued or running")
}

// CancelForPath cancels every unfinished job for a source, except keep. That
// includes one waiting to publish, whose output would no longer belong anywhere.
func (s *Store) CancelForPath(path, keep string) (int64, error) {
	res, err := s.db.Exec(`UPDATE jobs SET state = ?, updated = ?
		WHERE path = ? AND id != ? AND state IN ('queued', 'running', 'publishing')`,
		model.StateCancelled, s.now().Unix(), path, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Bump raises a queued job to the new-title priority.
func (s *Store) Bump(id string) error {
	res, err := s.db.Exec(`UPDATE jobs SET priority = ?, updated = ? WHERE id = ? AND state = 'queued'`,
		model.PriorityNew, s.now().Unix(), id)
	if err != nil {
		return err
	}
	return requireRow(res, id, "is not queued")
}

// Retry requeues a failed or cancelled job with fresh attempts.
func (s *Store) Retry(id string) error {
	res, err := s.db.Exec(`UPDATE jobs SET state = ?, attempts = 0, error = '', lease_id = '', not_before = 0, updated = ?
		WHERE id = ? AND state IN ('failed', 'cancelled')`,
		model.StateQueued, s.now().Unix(), id)
	if err != nil {
		return err
	}
	return requireRow(res, id, "is not failed or cancelled")
}

// HasActive reports whether a source has a job in progress.
func (s *Store) HasActive(path string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs
		WHERE path = ? AND state IN ('queued', 'running', 'publishing')`, path).Scan(&n)
	return n > 0, err
}

func requireRow(res sql.Result, id, why string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("job %s %s", id, why)
	}
	return nil
}
