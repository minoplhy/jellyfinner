package orchestrator

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
	"regexp"
	"strings"

	"jellyfinner/internal/pipeline"
	"jellyfinner/internal/queue"
)

// PublishPending handles every job waiting to be published.
func (s *Service) PublishPending(ctx context.Context) {
	jobs, err := s.Store.Publishing()
	if err != nil {
		s.Log.Printf("list publishing: %v", err)
		return
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return
		}
		s.publishOne(ctx, j)
	}
}

func (s *Service) publishOne(ctx context.Context, j *queue.Job) {
	incoming := filepath.Join(s.Cfg.IncomingDir, j.ID)
	video, err := s.publish(j, incoming)
	if err != nil {
		s.Log.Printf("publish %s: %v", j.ID, err)
		j.Error = "publish: " + err.Error()
		final, ferr := s.Store.PublishFailed(j.ID, j.Error, s.Cfg.MaxAttempts)
		if ferr != nil {
			s.Log.Printf("record publish failure %s: %v", j.ID, ferr)
			return
		}
		if rerr := os.RemoveAll(incoming); rerr != nil {
			s.Log.Printf("clear incoming %s: %v", j.ID, rerr)
		}
		if final {
			s.JobFailed(ctx, j)
		}
		return
	}

	if err := s.Store.SetEncoded(j.Path, j.SourceSize, j.SourceMtime, j.Profile, video.SHA256, video.Size); err != nil {
		s.Log.Printf("record encode %s: %v", j.ID, err)
		return
	}
	if err := s.Store.MarkDone(j.ID); err != nil {
		s.Log.Printf("mark done %s: %v", j.ID, err)
		return
	}
	if err := os.RemoveAll(incoming); err != nil {
		s.Log.Printf("clear incoming %s: %v", j.ID, err)
	}
	s.requestRefresh(ctx)
	if err := s.refreshPlacements(j.OutputPath, video); err != nil {
		s.Log.Printf("edge placements %s: %v", j.Path, err)
	}
	s.announce(ctx, j)
}

// announce names whoever asked for the title, and clears the played flag their
// stub left, or the finished title would show as already watched.
func (s *Service) announce(ctx context.Context, j *queue.Job) {
	requests, err := s.Store.TakeRequests(j.Path)
	if err != nil {
		s.Log.Printf("requests %s: %v", j.Path, err)
	}
	var names []string
	for _, r := range requests {
		if r.Username != "" {
			names = append(names, r.Username)
		}
		if s.Library == nil {
			continue
		}
		if err := s.Library.ClearPlayed(ctx, r.ItemID, r.UserID); err != nil {
			s.Log.Printf("clear played %s: %v", j.Path, err)
		}
	}
	message := "Ready to watch: " + Title(j.Path)
	if len(names) > 0 {
		message += " (requested by " + strings.Join(names, ", ") + ")"
	}
	s.send(ctx, message)
}

// publish checks the upload against its manifest, then renames it into the
// library. Sidecars go first, so subtitles are there when the video appears.
// The rename replaces any previous encode in one step.
func (s *Service) publish(j *queue.Job, incoming string) (pipeline.File, error) {
	var none pipeline.File
	var m pipeline.Manifest
	data, err := os.ReadFile(filepath.Join(incoming, pipeline.ManifestName))
	if err != nil {
		return none, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return none, fmt.Errorf("manifest: %w", err)
	}
	if m.JobID != j.ID {
		return none, fmt.Errorf("manifest is for job %s", m.JobID)
	}

	// The output path comes from the database, never from the upload.
	video := path.Base(j.OutputPath)
	var sidecars []string
	var videoFile pipeline.File
	foundVideo := false
	for _, f := range m.Files {
		if f.Name != filepath.Base(f.Name) || f.Name == "." || f.Name == ".." {
			return none, fmt.Errorf("manifest: unsafe file name %q", f.Name)
		}
		if err := checkFile(filepath.Join(incoming, f.Name), f); err != nil {
			return none, err
		}
		if f.Name == video {
			foundVideo = true
			videoFile = f
		} else {
			sidecars = append(sidecars, f.Name)
		}
	}
	if !foundVideo {
		return none, fmt.Errorf("manifest does not list %q", video)
	}

	target := filepath.Join(s.Cfg.LibraryDir, filepath.FromSlash(j.OutputPath))
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return none, err
	}
	stale, err := sidecarsOf(target)
	if err != nil {
		return none, err
	}
	for _, name := range append(sidecars, video) {
		src := filepath.Join(incoming, name)
		if err := os.Chmod(src, 0o644); err != nil {
			return none, err
		}
		if err := os.Rename(src, filepath.Join(dir, name)); err != nil {
			return none, err
		}
	}
	// A re-encode can drop a subtitle track, so old sidecars must not linger.
	for _, old := range stale {
		if !contains(sidecars, filepath.Base(old)) {
			if err := os.Remove(old); err != nil {
				return none, err
			}
		}
	}
	return videoFile, nil
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func checkFile(file string, want pipeline.File) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if size != want.Size {
		return fmt.Errorf("%s: size %d, manifest says %d", want.Name, size, want.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want.SHA256 {
		return fmt.Errorf("%s: checksum mismatch", want.Name)
	}
	return nil
}

// sidecarPattern matches what follows the stem in "<stem>.<lang>.srt" and
// "<stem>.<lang>.<n>.srt", or the same with .ass, so a title never claims a
// neighbour's subtitles.
var sidecarPattern = regexp.MustCompile(`^\.[A-Za-z]{2,8}(\.[0-9]+)?\.(srt|ass)$`)

// sidecarsOf lists the subtitle files belonging to a video.
func sidecarsOf(video string) ([]string, error) {
	dir := filepath.Dir(video)
	stem := strings.TrimSuffix(filepath.Base(video), filepath.Ext(video))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if rest, ok := strings.CutPrefix(e.Name(), stem); ok && sidecarPattern.MatchString(rest) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}

func (s *Service) removeOutput(outputPath string) error {
	video := filepath.Join(s.Cfg.LibraryDir, filepath.FromSlash(outputPath))
	sidecars, err := sidecarsOf(video)
	if err != nil {
		return err
	}
	for _, f := range append(sidecars, video) {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Service) moveOutput(from, to string) error {
	src := filepath.Join(s.Cfg.LibraryDir, filepath.FromSlash(from))
	dst := filepath.Join(s.Cfg.LibraryDir, filepath.FromSlash(to))
	sidecars, err := sidecarsOf(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	oldStem := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	newStem := strings.TrimSuffix(filepath.Base(dst), filepath.Ext(dst))
	for _, f := range sidecars {
		name := newStem + strings.TrimPrefix(filepath.Base(f), oldStem)
		if err := os.Rename(f, filepath.Join(filepath.Dir(dst), name)); err != nil {
			return err
		}
	}
	return os.Rename(src, dst)
}
