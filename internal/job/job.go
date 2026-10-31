// Package job defines the unit of work handed to the encoding node.
package job

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"

	"jellyfinner/internal/encode"
)

// Job describes one title to encode. With rclone transfers, Source and Incoming
// are rclone paths, such as "origin:/srv/originals/Movies/x.mkv", or local
// paths. With control API transfers they are only informational.
type Job struct {
	ID          string `json:"id"`
	Source      string `json:"source"`
	Incoming    string `json:"incoming"`
	OutputPath  string `json:"output_path"`
	Revision    int    `json:"revision"`
	Reason      string `json:"reason,omitempty"`
	ForceEncode bool   `json:"force_encode,omitempty"`
	Software    bool   `json:"software,omitempty"`

	// Profile is set by the orchestrator, so output settings live in one place.
	Profile encode.Profile `json:"profile"`
}

// ComputeID derives the dedupe key: the same source version and settings map
// to the same job, while a re-encode, which bumps revision, does not.
func ComputeID(path string, size, mtimeNS int64, profile string, revision int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d\x00%s\x00%d", path, size, mtimeNS, profile, revision))
	return hex.EncodeToString(sum[:8])
}

// Validate rejects jobs that could write outside the job's incoming directory
// or produce something other than a direct-play MP4.
func (j *Job) Validate() error {
	if j.ID == "" {
		return errors.New("job: missing id")
	}
	if strings.ContainsAny(j.ID, `/\`) || j.ID == "." || j.ID == ".." {
		return fmt.Errorf("job: invalid id %q", j.ID)
	}
	if j.Source == "" {
		return errors.New("job: missing source")
	}
	if j.Incoming == "" {
		return errors.New("job: missing incoming")
	}
	if j.Revision < 0 {
		return errors.New("job: negative revision")
	}
	return validateOutputPath(j.OutputPath)
}

func validateOutputPath(p string) error {
	if p == "" {
		return errors.New("job: missing output path")
	}
	if strings.Contains(p, `\`) || path.IsAbs(p) || path.Clean(p) != p {
		return fmt.Errorf("job: output path %q must be a clean relative path", p)
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return fmt.Errorf("job: output path %q escapes the library", p)
	}
	if path.Ext(p) != ".mp4" {
		return fmt.Errorf("job: output path %q must end in .mp4", p)
	}
	return nil
}
