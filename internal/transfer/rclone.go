package transfer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"jellyfinner/internal/job"
)

// Rclone wraps the rclone binary. Streams is tuned for long paths, where a
// single TCP stream cannot fill the link.
type Rclone struct {
	Binary  string
	Streams int
	Log     io.Writer
}

// Size returns the size of the job's source.
func (r *Rclone) Size(ctx context.Context, j *job.Job) (int64, error) {
	return r.size(ctx, j.Source)
}

// Pull copies the job's source to a local path.
func (r *Rclone) Pull(ctx context.Context, j *job.Job, local string) error {
	return r.copy(ctx, j.Source, local)
}

// Push copies a local file into the job's incoming folder.
func (r *Rclone) Push(ctx context.Context, j *job.Job, local, name string) error {
	return r.copy(ctx, local, strings.TrimSuffix(j.Incoming, "/")+"/"+j.ID+"/"+name)
}

// copy copies one file. src and dst are rclone paths or local paths.
func (r *Rclone) copy(ctx context.Context, src, dst string) error {
	cmd := exec.CommandContext(ctx, r.Binary, r.copyArgs(src, dst)...)
	cmd.Stdout = r.Log
	cmd.Stderr = r.Log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rclone copy %s: %w", src, err)
	}
	return nil
}

func (r *Rclone) copyArgs(src, dst string) []string {
	return []string{
		"copyto", src, dst,
		"--multi-thread-streams", strconv.Itoa(r.Streams),
		"--multi-thread-cutoff", "64M",
		"--retries", "5",
		"--low-level-retries", "20",
	}
}

func (r *Rclone) size(ctx context.Context, src string) (int64, error) {
	cmd := exec.CommandContext(ctx, r.Binary, "lsjson", "--stat", src)
	cmd.Stderr = r.Log
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("rclone stat %s: %w", src, err)
	}
	var info struct {
		Size int64 `json:"Size"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return 0, fmt.Errorf("rclone stat %s: %w", src, err)
	}
	return info.Size, nil
}
