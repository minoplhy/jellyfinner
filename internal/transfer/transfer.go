// Package transfer moves a job's files between the orchestrator and a worker.
package transfer

import (
	"context"

	"jellyfinner/internal/job"
)

// Transfer moves one job's files. Pull must keep a fully pulled source in
// place, so a retry can skip it.
type Transfer interface {
	Size(ctx context.Context, j *job.Job) (int64, error)
	Pull(ctx context.Context, j *job.Job, local string) error
	Push(ctx context.Context, j *job.Job, local, name string) error
}
