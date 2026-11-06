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
	// OnBackend, when set, hears which encoder each attempt starts with:
	// the configured one, software after a fall back, or BackendCopy.
	OnBackend func(encode.Backend)
}

// BackendCopy is what OnBackend hears for a stream copy, which encodes nothing.
const BackendCopy encode.Backend = "copy"

// Timing records when the encode ran on the worker's clock, and how long each
// stage took. A retry that reuses an earlier verified encode keeps that
// encode's times and sets Reused.
type Timing struct {
	EncodeStarted  time.Time `json:"encode_started,omitzero"`
	EncodeFinished time.Time `json:"encode_finished,omitzero"`
	EncodeSeconds  float64   `json:"encode_seconds,omitempty"`
	PullSeconds    float64   `json:"pull_seconds,omitempty"`
	PushSeconds    float64   `json:"push_seconds,omitempty"`
	Reused         bool      `json:"reused,omitempty"`
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
	Timing     *Timing        `json:"timing,omitempty"`
}

// Progress receives the current stage, and completion from 0 to 1 when known.
type Progress func(stage string, fraction float64)

// Run executes a job. Success or cancellation clears scratch. A failure or a
// worker shutdown keeps the pulled source and any verified encode, so a retry
// on this node skips the pull, and after a failed push skips the encode too.
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
	pullSeconds := time.Since(started).Seconds()

	// The job decides the output, and the node only decides its hardware.
	profile := cfg.Profile
	if j.Profile.MaxHeight > 0 {
		device := profile.Device
		profile = j.Profile
		profile.Device = device
	}
	base := path.Base(j.OutputPath)
	outDir := filepath.Join(dir, "out")
	key := reuseKey(cfg, j, profile, base, size)
	done := loadEncoded(outDir, key)
	if done != nil {
		fmt.Fprintln(log, "reusing the encode from an earlier attempt")
		done.Reused = true
	} else {
		encodeStarted := time.Now()
		done, err = encodeAll(ctx, cfg, j, profile, src, outDir, base, log, progress)
		if err != nil {
			return nil, err
		}
		encodeFinished := time.Now()
		done.Key = key
		done.EncodeStarted, done.EncodeFinished = encodeStarted.UTC(), encodeFinished.UTC()
		done.EncodeSeconds = encodeFinished.Sub(encodeStarted).Seconds()
		if err := saveEncoded(outDir, done); err != nil {
			return nil, err
		}
	}

	progress("push", 0)
	manifest := &Manifest{
		JobID:      j.ID,
		OutputPath: j.OutputPath,
		Backend:    done.Backend,
		VideoCopy:  done.VideoCopy,
		Skipped:    done.Skipped,
		Timing: &Timing{
			EncodeStarted:  done.EncodeStarted,
			EncodeFinished: done.EncodeFinished,
			EncodeSeconds:  done.EncodeSeconds,
			PullSeconds:    pullSeconds,
			Reused:         done.Reused,
		},
	}
	started = time.Now()
	if err := push(ctx, cfg, j, outDir, done.Names, manifest); err != nil {
		return nil, err
	}
	var pushed int64
	for _, f := range manifest.Files {
		pushed += f.Size
	}
	logRate(log, "push", pushed, started)
	manifest.Timing.PushSeconds = time.Since(started).Seconds()
	progress("done", 1)
	succeeded = true
	return manifest, nil
}

// encodeAll probes, encodes and extracts subtitles into a fresh outDir. The
// result is verified, and lists every file to push with the main file last.
func encodeAll(ctx context.Context, cfg Config, j *job.Job, profile encode.Profile, src, outDir, base string, log io.Writer, progress Progress) (*encoded, error) {
	probe, err := encode.RunProbe(ctx, cfg.FFprobe, src)
	if err != nil {
		return nil, err
	}
	plan, err := encode.NewPlan(probe, profile, j.ForceEncode)
	if err != nil {
		return nil, err
	}
	if err := os.RemoveAll(outDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return nil, err
	}
	backend, err := encodeVideo(ctx, cfg, j, profile, src, filepath.Join(outDir, base), plan, log, progress)
	if err != nil {
		return nil, err
	}
	sidecars, err := extractSubtitles(ctx, cfg, src, outDir, base, plan, log)
	if err != nil {
		return nil, err
	}
	return &encoded{
		Names:     append(sidecars, base),
		Backend:   backend,
		VideoCopy: plan.Video.Copy,
		Skipped:   plan.Skipped,
	}, nil
}

// encodedName marks a verified encode in the output folder, so a retry after a
// failed push sends it again instead of encoding the title again.
const encodedName = "encoded.json"

type encoded struct {
	Key       string         `json:"key"`
	Names     []string       `json:"names"`
	Backend   encode.Backend `json:"backend,omitempty"`
	VideoCopy bool           `json:"video_copy"`
	Skipped   []string       `json:"skipped,omitempty"`
	// When the encode ran, so a retry that reuses it reports the real times.
	EncodeStarted  time.Time `json:"encode_started,omitzero"`
	EncodeFinished time.Time `json:"encode_finished,omitzero"`
	EncodeSeconds  float64   `json:"encode_seconds,omitempty"`
	Reused         bool      `json:"-"`
}

// reuseKey covers everything that changes the output for the same job ID, which
// already pins the source version and revision.
func reuseKey(cfg Config, j *job.Job, profile encode.Profile, base string, sourceSize int64) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%t\x00%t\x00%s\x00%d",
		profile.Hash(), cfg.Backend, j.ForceEncode, j.Software, base, sourceSize))
	return hex.EncodeToString(sum[:])
}

// loadEncoded returns the earlier encode only if it matches key and every file
// it lists is still there.
func loadEncoded(outDir, key string) *encoded {
	data, err := os.ReadFile(filepath.Join(outDir, encodedName))
	if err != nil {
		return nil
	}
	var e encoded
	if err := json.Unmarshal(data, &e); err != nil || e.Key != key || len(e.Names) == 0 {
		return nil
	}
	for _, name := range e.Names {
		if !filepath.IsLocal(name) || name == ManifestName || name == encodedName {
			return nil
		}
		if info, err := os.Stat(filepath.Join(outDir, name)); err != nil || !info.Mode().IsRegular() {
			return nil
		}
	}
	return &e
}

// saveEncoded writes the marker by rename, so a crash never leaves a partial one.
func saveEncoded(outDir string, e *encoded) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp := filepath.Join(outDir, encodedName+".tmp")
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(outDir, encodedName))
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
		if cfg.OnBackend != nil {
			if plan.Video.Copy {
				cfg.OnBackend(BackendCopy)
			} else {
				cfg.OnBackend(backend)
			}
		}
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
