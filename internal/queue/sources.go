package queue

import (
	"database/sql"
	"errors"
)

// Source is one file tracked under the originals directory. The Encoded fields
// record the version that was last published.
type Source struct {
	Path           string
	Size           int64
	MtimeNS        int64
	LastChanged    int64
	Backlog        bool
	Revision       int
	EncodedSize    int64
	EncodedMtimeNS int64
	EncodedProfile string
	EncodedSHA256  string
	EncodedBytes   int64
	MissingSince   int64
}

// Encoded reports whether the current version has been published.
func (s *Source) Encoded() bool {
	return s.EncodedSize == s.Size && s.EncodedMtimeNS == s.MtimeNS
}

const sourceColumns = `path, size, mtime_ns, last_changed, backlog, revision,
	encoded_size, encoded_mtime_ns, encoded_profile, encoded_sha256, encoded_bytes, missing_since`

func scanSource(row interface{ Scan(...any) error }) (*Source, error) {
	var s Source
	err := row.Scan(&s.Path, &s.Size, &s.MtimeNS, &s.LastChanged, &s.Backlog, &s.Revision,
		&s.EncodedSize, &s.EncodedMtimeNS, &s.EncodedProfile, &s.EncodedSHA256, &s.EncodedBytes, &s.MissingSince)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Sources returns every tracked source, ordered by path.
func (s *Store) Sources() ([]*Source, error) {
	rows, err := s.db.Query(`SELECT ` + sourceColumns + ` FROM sources ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// Source returns one tracked source.
func (s *Store) Source(path string) (*Source, error) {
	src, err := scanSource(s.db.QueryRow(`SELECT `+sourceColumns+` FROM sources WHERE path = ?`, path))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return src, err
}

// AddSource starts tracking a file.
func (s *Store) AddSource(path string, size, mtimeNS int64, backlog bool) error {
	_, err := s.db.Exec(`INSERT INTO sources (path, size, mtime_ns, last_changed, backlog) VALUES (?, ?, ?, ?, ?)`,
		path, size, mtimeNS, s.Now().Unix(), backlog)
	return err
}

// UpdateSource records a changed file and restarts its stability wait. A
// replaced file counts as new work, so it leaves the backlog.
func (s *Store) UpdateSource(path string, size, mtimeNS int64) error {
	_, err := s.db.Exec(`UPDATE sources SET size = ?, mtime_ns = ?, last_changed = ?, backlog = 0, missing_since = 0
		WHERE path = ?`, size, mtimeNS, s.Now().Unix(), path)
	return err
}

// SetMissing records when a file disappeared, or clears it with 0.
func (s *Store) SetMissing(path string, since int64) error {
	_, err := s.db.Exec(`UPDATE sources SET missing_since = ? WHERE path = ?`, since, path)
	return err
}

// RenameSource moves a row to a new path, keeping its encode record.
func (s *Store) RenameSource(from, to string) error {
	_, err := s.db.Exec(`UPDATE sources SET path = ?, missing_since = 0 WHERE path = ?`, to, from)
	return err
}

// DeleteSource stops tracking a file.
func (s *Store) DeleteSource(path string) error {
	_, err := s.db.Exec(`DELETE FROM sources WHERE path = ?`, path)
	return err
}

// SetEncoded records the source version that was just published.
func (s *Store) SetEncoded(path string, size, mtimeNS int64, profile, sha256 string, bytes int64) error {
	_, err := s.db.Exec(`UPDATE sources SET encoded_size = ?, encoded_mtime_ns = ?, encoded_profile = ?,
		encoded_sha256 = ?, encoded_bytes = ?, backlog = 0 WHERE path = ?`,
		size, mtimeNS, profile, sha256, bytes, path)
	return err
}

// BumpRevision increments and returns a source's revision, which gives a
// re-encode a new job ID.
func (s *Store) BumpRevision(path string) (int, error) {
	var revision int
	err := s.db.QueryRow(`UPDATE sources SET revision = revision + 1 WHERE path = ? RETURNING revision`, path).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return revision, err
}
