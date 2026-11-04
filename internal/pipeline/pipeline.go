// Package pipeline runs one job on the encoding node: pull, plan, encode,
// verify, push.
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"jellyfinner/internal/encode"
	"jellyfinner/internal/job"
	"jellyfinner/internal/transfer"
)

// ManifestName is pushed last, so its presence means the upload is complete.
const ManifestName = "manifest.json"

// ErrShutdown is the cancel cause when the worker itself is stopping. The job
// will run again, so scratch stays for the retry.
var ErrShutdown = errors.New("worker shutting down")

// Scratch must hold the source plus the output. Outputs are capped well below
// most sources, so half the source size again is a safe margin.
const scratchFactor = 1.5

// Config holds the worker's fixed settings.
type Config struct {
	Scratch  string
	FFmpeg   string
	FFprobe  string
	Profile  encode.Profile
	Backend  encode.Backend
	Transfer transfer.Transfer
}

// File is one pushed file, with the checksum B verifies before publishing.
type File struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest describes a finished job's upload.
type Manifest struct {
	JobID      string         `json:"job_id"`
	OutputPath string         `json:"output_path"`
	Backend    encode.Backend `json:"backend,omitempty"`
	VideoCopy  bool           `json:"video_copy"`
	Skipped    []string       `json:"skipped,omitempty"`
	Files      []File         `json:"files"`
}

// Progress receives the current stage, and completion from 0 to 1 when known.
type Progress func(stage string, fraction float64)

// Run executes a job. Success or cancellation clears scratch. A failure or a
// worker shutdown keeps the pulled source, so a retry on this node skips the
// transfer.
func Run(ctx context.Context, cfg Config, j *job.Job, log io.Writer, progress Progress) (*Manifest, error) {
	if err := j.Validate(); err != nil {
		return nil, err
	}
	if progress == nil {
		progress = func(string, float64) {}
	}

	dir := filepath.Join(cfg.Scratch, j.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	succeeded := false
	defer func() {
		if succeeded || (ctx.Err() != nil && !errors.Is(context.Cause(ctx), ErrShutdown)) {
			os.RemoveAll(dir)
		}
	}()

	size, err := cfg.Transfer.Size(ctx, j)
	if err != nil {
		return nil, err
	}
	src := filepath.Join(dir, "source"+path.Ext(j.Source))
	var held int64
	if info, err := os.Stat(src); err == nil {
		held = info.Size()
	}
	if err := checkScratch(cfg.Scratch, size, held); err != nil {
		return nil, err
	}

	progress("pull", 0)
	started := time.Now()
	if err := cfg.Transfer.Pull(ctx, j, src); err != nil {
		return nil, err
	}
	logRate(log, "pull", size, started)

	probe, err := encode.RunProbe(ctx, cfg.FFprobe, src)
	if err != nil {
		return nil, err
	}
	// The job decides the output, and the node only decides its hardware.
	profile := cfg.Profile
	if j.Profile.MaxHeight > 0 {
		device := profile.Device
		profile = j.Profile
		profile.Device = device
	}
	plan, err := encode.NewPlan(probe, profile, j.ForceEncode)
	if err != nil {
		return nil, err
	}

	outDir := filepath.Join(dir, "out")
	if err := os.RemoveAll(outDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return nil, err
	}
	base := path.Base(j.OutputPath)
	out := filepath.Join(outDir, base)
	backend, err := encodeVideo(ctx, cfg, j, profile, src, out, plan, log, progress)
	if err != nil {
		return nil, err
	}

	sidecars, err := extractSubtitles(ctx, cfg, src, outDir, base, plan, log)
	if err != nil {
		return nil, err
	}

	progress("push", 0)
	manifest := &Manifest{
		JobID:      j.ID,
		OutputPath: j.OutputPath,
		Backend:    backend,
		VideoCopy:  plan.Video.Copy,
		Skipped:    plan.Skipped,
	}
	started = time.Now()
	if err := push(ctx, cfg, j, outDir, append(sidecars, base), manifest); err != nil {
		return nil, err
	}
	var pushed int64
	for _, f := range manifest.Files {
		pushed += f.Size
	}
	logRate(log, "push", pushed, started)
	progress("done", 1)
	succeeded = true
	return manifest, nil
}

// logRate records transfer speed in the job's log, which is how the transfer
// methods are compared on a real link.
func logRate(log io.Writer, stage string, size int64, started time.Time) {
	seconds := max(time.Since(started).Seconds(), 0.001)
	fmt.Fprintf(log, "%s: %.1f MB in %.1fs, %.1f MB/s\n", stage, float64(size)/1e6, seconds, float64(size)/1e6/seconds)
}

// encodeVideo tries the configured backend, then falls back to software. It
// returns the backend that produced a verified file, or "" for a stream copy.
func encodeVideo(ctx context.Context, cfg Config, j *job.Job, profile encode.Profile, src, out string, plan *encode.Plan, log io.Writer, progress Progress) (encode.Backend, error) {
	report := func(f float64) { progress("encode", f) }
	verify := func() error {
		return encode.Verify(ctx, cfg.FFmpeg, cfg.FFprobe, out, plan, log)
	}
	attempt := func(backend encode.Backend) error {
		args := encode.EncodeArgs(src, out, plan, profile, backend)
		if err := encode.Run(ctx, cfg.FFmpeg, args, plan.Duration, log, report); err != nil {
			return err
		}
		return verify()
	}

	if plan.Video.Copy {
		return "", attempt(cfg.Backend)
	}
	backends := []encode.Backend{cfg.Backend}
	if j.Software {
		backends = []encode.Backend{encode.BackendSoftware}
	} else if cfg.Backend != encode.BackendSoftware {
		backends = append(backends, encode.BackendSoftware)
	}
	var errs []error
	for _, backend := range backends {
		err := attempt(backend)
		if err == nil {
			return backend, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		fmt.Fprintf(log, "backend %s failed: %v\n", backend, err)
		errs = append(errs, fmt.Errorf("%s: %w", backend, err))
	}
	return "", errors.Join(errs...)
}

func extractSubtitles(ctx context.Context, cfg Config, src, outDir, base string, plan *encode.Plan, log io.Writer) ([]string, error) {
	if len(plan.Subs) == 0 {
		return nil, nil
	}
	stem := strings.TrimSuffix(base, path.Ext(base))
	seen := map[string]int{}
	names := make([]string, len(plan.Subs))
	paths := make([]string, len(plan.Subs))
	for i, s := range plan.Subs {
		names[i] = encode.SidecarName(stem, s.Lang, seen[s.Lang])
		paths[i] = filepath.Join(outDir, names[i])
		seen[s.Lang]++
	}
	args := encode.SubtitleArgs(src, plan, paths)
	if err := encode.Run(ctx, cfg.FFmpeg, args, 0, log, nil); err != nil {
		return nil, fmt.Errorf("extract subtitles: %w", err)
	}
	for i, s := range plan.Subs {
		if s.Styled {
			names = append(names, encode.StyledName(names[i]))
		}
	}
	return names, nil
}

// push uploads into a job-specific directory, ending with the manifest. A
// crash before the manifest leaves nothing B will publish.
func push(ctx context.Context, cfg Config, j *job.Job, outDir string, names []string, m *Manifest) error {
	for _, name := range names {
		local := filepath.Join(outDir, name)
		f, err := describe(local, name)
		if err != nil {
			return err
		}
		m.Files = append(m.Files, f)
		if err := cfg.Transfer.Push(ctx, j, local, name); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	manifest := filepath.Join(outDir, ManifestName)
	if err := os.WriteFile(manifest, data, 0o640); err != nil {
		return err
	}
	return cfg.Transfer.Push(ctx, j, manifest, ManifestName)
}

func describe(file, name string) (File, error) {
	f, err := os.Open(file)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return File{}, err
	}
	return File{Name: name, Size: size, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// checkScratch allows for held, the part of the source already on disk.
func checkScratch(dir string, sourceSize, held int64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("check scratch: %w", err)
	}
	free := int64(st.Bavail) * int64(st.Bsize)
	need := int64(float64(sourceSize)*scratchFactor) - held
	if free < need {
		return fmt.Errorf("scratch has %d bytes free, need %d", free, need)
	}
	return nil
}
