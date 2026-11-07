package orchestrator

import (
	"strings"

	"jellyfinner/internal/config"
	"jellyfinner/internal/model"
)

// Title states shown to operators.
const (
	TitleReady    = "ready"
	TitleStub     = "stub"
	TitleBacklog  = "backlog"
	TitleWaiting  = "waiting"
	TitleMissing  = "missing"
	TitleFailed   = "failed"
	TitleQueued   = "queued"
	TitleEncoding = "encoding"
)

// TitleRow is one line of the library view.
type TitleRow struct {
	Path  string
	Title string
	State string
	Size  int64
	// EncodedSize is the published output's size, 0 if none was published.
	EncodedSize int64
	Job         *model.Job
}

// Titles lists titles matching a case-insensitive search.
func (s *Service) Titles(search string, limit int) ([]TitleRow, int, error) {
	sources, err := s.Store.Sources()
	if err != nil {
		return nil, 0, err
	}
	jobs, err := s.Store.LatestJobs()
	if err != nil {
		return nil, 0, err
	}
	search = strings.ToLower(search)
	var rows []TitleRow
	total := 0
	for _, src := range sources {
		if search != "" && !strings.Contains(strings.ToLower(src.Path), search) {
			continue
		}
		total++
		if len(rows) < limit {
			rows = append(rows, s.titleRow(src, jobs[src.Path]))
		}
	}
	return rows, total, nil
}

func (s *Service) titleRow(src *model.Source, j *model.Job) TitleRow {
	return TitleRow{Path: src.Path, Title: Title(src.Path), Size: src.Size, EncodedSize: src.EncodedBytes, Job: j, State: s.titleState(src, j)}
}

func (s *Service) titleState(src *model.Source, j *model.Job) string {
	if src.MissingSince != 0 {
		return TitleMissing
	}
	if j != nil {
		switch j.State {
		case model.StateQueued:
			return TitleQueued
		case model.StateRunning, model.StatePublishing:
			return TitleEncoding
		}
	}
	switch {
	case src.Encoded():
		return TitleReady
	case j != nil && j.State == model.StateFailed && j.SourceSize == src.Size && j.SourceMtime == src.MtimeNS:
		return TitleFailed
	case s.Cfg.Mode == config.ModeManual:
		return TitleStub
	case src.Backlog:
		return TitleBacklog
	}
	return TitleWaiting
}

// SourceSizes returns the total size of the named sources, for confirming a
// re-encode, and skips unknown paths.
func (s *Service) SourceSizes(paths []string) (int64, []string, error) {
	var total int64
	var known []string
	for _, p := range paths {
		src, err := s.Store.Source(p)
		if err != nil {
			continue
		}
		total += src.Size
		known = append(known, p)
	}
	return total, known, nil
}
