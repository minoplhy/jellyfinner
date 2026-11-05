// Package orchestrator runs the central side: it turns watched files into jobs,
// reaps dead workers, and publishes finished encodes.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"jellyfinner/internal/config"
	"jellyfinner/internal/database"
	"jellyfinner/internal/jellyfin"
	"jellyfinner/internal/job"
	"jellyfinner/internal/model"
	"jellyfinner/internal/nodes"
	"jellyfinner/internal/notify"
	"jellyfinner/internal/stub"
	"jellyfinner/internal/watch"
)

// massDeleteMin stops the percentage check tripping on small libraries.
const massDeleteMin = 5

const reapInterval = 15 * time.Second

// StubName is the master stub inside INCOMING_DIR, which shares a filesystem
// with the library but is not one.
const StubName = ".stub.mp4"

// Service holds the orchestrator's background work.
type Service struct {
	Cfg     *config.Config
	Store   database.Store
	Nodes   *nodes.Registry
	Now     func() time.Time // the clock; the store should share it
	Notify  notify.Notifier
	Library jellyfin.API // nil when no Jellyfin URL is set
	Stub    stub.Master
	Log     *log.Logger
	Profile string
	// Cloud rents workers on demand; nil unless CLOUD_PROVIDERS lists any.
	Cloud *Cloud

	publishNow chan struct{}
	scanNow    chan struct{}

	mu           sync.Mutex
	refreshTimer *time.Timer
	scanFailing  bool
	deletePaused bool
}

// New returns a Service. The profile hash goes into every job ID, so changing
// the output profile produces new jobs rather than matching old ones.
func New(cfg *config.Config, store database.Store, n notify.Notifier, library jellyfin.API, logger *log.Logger) *Service {
	s := &Service{
		Cfg:        cfg,
		Store:      store,
		Now:        time.Now,
		Notify:     n,
		Library:    library,
		Log:        logger,
		Profile:    cfg.Output.Hash(),
		Stub:       stub.Master(filepath.Join(cfg.IncomingDir, StubName)),
		publishNow: make(chan struct{}, 1),
		scanNow:    make(chan struct{}, 1),
	}
	// Through a closure, so a clock swapped in later reaches the cache too.
	s.Nodes = nodes.New(store, func() time.Time { return s.Now() })
	return s
}

// CheckMode records the mode on first run and refuses a later change, because
// changing a .env file alone must never start a backfill.
func CheckMode(meta database.MetaStore, mode string) error {
	stored, err := meta.Meta(model.MetaActiveMode)
	if err != nil {
		return err
	}
	if stored == "" {
		return meta.SetMeta(model.MetaActiveMode, mode)
	}
	if stored != mode {
		return fmt.Errorf("ENCODE_MODE is %q but this database runs %q: change it with an explicit mode switch", mode, stored)
	}
	return nil
}

// SourceByOutput finds the source that produces a library path.
func (s *Service) SourceByOutput(output string) (*model.Source, error) {
	sources, err := s.Store.Sources()
	if err != nil {
		return nil, err
	}
	for _, src := range sources {
		if OutputPath(src.Path) == output {
			return src, nil
		}
	}
	return nil, model.ErrNotFound
}

// Run blocks until ctx ends.
func (s *Service) Run(ctx context.Context) {
	scan := time.NewTicker(s.Cfg.ScanInterval)
	defer scan.Stop()
	reap := time.NewTicker(reapInterval)
	defer reap.Stop()
	probe := time.NewTicker(probeInterval)
	defer probe.Stop()
	defer s.stopRefreshTimer()

	s.TriggerScan()
	s.TriggerPublish()
	var cloudDone sync.WaitGroup
	defer cloudDone.Wait()
	if s.Cloud != nil {
		cloudDone.Add(1)
		go func() {
			defer cloudDone.Done()
			s.Cloud.Run(ctx)
		}()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-scan.C:
			s.runScan(ctx)
		case <-s.scanNow:
			s.runScan(ctx)
		case <-reap.C:
			s.reap(ctx)
		case <-probe.C:
			s.probeNodes(ctx)
		case <-s.publishNow:
			s.PublishPending(ctx)
		}
	}
}

// TriggerScan requests a scan without waiting. It is the fast path for
// anything that knows a file arrived; the timed scan remains the source of truth.
func (s *Service) TriggerScan() {
	select {
	case s.scanNow <- struct{}{}:
	default:
	}
}

// TriggerPublish requests a publish pass without waiting.
func (s *Service) TriggerPublish() {
	select {
	case s.publishNow <- struct{}{}:
	default:
	}
}

func (s *Service) send(ctx context.Context, message string) {
	s.Log.Print(message)
	if err := s.Notify.Send(ctx, message); err != nil {
		s.Log.Printf("notify: %v", err)
	}
}

func (s *Service) reap(ctx context.Context) {
	failed, err := s.Store.ExpireLeases(s.Cfg.MaxAttempts)
	if err != nil {
		s.Log.Printf("expire leases: %v", err)
		return
	}
	for _, j := range failed {
		s.JobFailed(ctx, j)
	}
	s.clearAbandonedUploads()
}

// clearAbandonedUploads removes what failed and cancelled jobs left in
// INCOMING_DIR. Only publishing clears a job's folder otherwise. A folder with
// no job stays, because nothing proves what it is.
func (s *Service) clearAbandonedUploads() {
	entries, err := os.ReadDir(s.Cfg.IncomingDir)
	if err != nil {
		s.Log.Printf("list incoming: %v", err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		j, err := s.Store.Get(e.Name())
		if err != nil {
			if !errors.Is(err, model.ErrNotFound) {
				s.Log.Printf("incoming %s: %v", e.Name(), err)
			}
			continue
		}
		if j.State != model.StateFailed && j.State != model.StateCancelled {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.Cfg.IncomingDir, e.Name())); err != nil {
			s.Log.Printf("clear incoming %s: %v", e.Name(), err)
		}
	}
}

// JobFailed reports a job that has run out of attempts.
func (s *Service) JobFailed(ctx context.Context, j *model.Job) {
	message := fmt.Sprintf("Encode failed: %s (job %s, %d attempts). Last error: %s",
		Title(j.Path), j.ID, j.Attempts, j.Error)
	if s.Cfg.AdminURL != "" {
		message += " " + s.Cfg.AdminURL + "/jobs/" + url.PathEscape(j.ID)
	}
	s.send(ctx, message)
}

func (s *Service) runScan(ctx context.Context) {
	err := s.ScanOnce(ctx)
	s.mu.Lock()
	wasFailing := s.scanFailing
	s.scanFailing = err != nil
	s.mu.Unlock()
	switch {
	case err != nil && !wasFailing:
		s.send(ctx, fmt.Sprintf("Originals scan failing, no changes applied: %v", err))
	case err != nil:
		s.Log.Printf("scan: %v", err)
	case wasFailing:
		s.send(ctx, "Originals scan recovered.")
	}
}

// ScanOnce applies one scan to the database and queues anything ready.
func (s *Service) ScanOnce(ctx context.Context) error {
	files, err := watch.Scan(s.Cfg.OriginalsDir)
	if err != nil {
		return err
	}
	tracked, err := s.Store.Sources()
	if err != nil {
		return err
	}
	if len(files) == 0 && len(tracked) > 0 {
		return fmt.Errorf("%s looks empty but %d sources are tracked, is the disk mounted?", s.Cfg.OriginalsDir, len(tracked))
	}

	// Files found by the very first scan are the existing library, not new
	// arrivals. They wait as backlog rather than all starting at once.
	scanned, err := s.Store.Meta(model.MetaFirstScanDone)
	if err != nil {
		return err
	}
	firstScan := scanned == ""

	changes := watch.Classify(tracked, files)
	for _, r := range changes.Renamed {
		if err := s.applyRename(r); err != nil {
			s.Log.Printf("rename %s: %v", r.From.Path, err)
		}
	}
	for _, p := range changes.New {
		f := files[p]
		if err := s.Store.AddSource(p, f.Size, f.MtimeNS, firstScan); err != nil {
			return err
		}
	}
	// Replaced files are new work, but a restore that changes every mtime looks
	// the same, and must not spend the whole library's traffic at once.
	held := s.massive(len(changes.Changed), len(tracked))
	for _, p := range changes.Changed {
		f := files[p]
		if err := s.Store.UpdateSource(p, f.Size, f.MtimeNS); err != nil {
			return err
		}
		if held {
			if err := s.Store.SetBacklog(p); err != nil {
				return err
			}
		}
		if _, err := s.Store.CancelForPath(p, ""); err != nil {
			return err
		}
	}
	if held {
		s.send(ctx, fmt.Sprintf("Changes held: %d of %d sources changed at once, so they wait in the backlog. Check the originals disk. Backfill encodes them within its budget, or use queue add.", len(changes.Changed), len(tracked)))
	}
	for _, p := range changes.Returned {
		if err := s.Store.SetMissing(p, 0); err != nil {
			return err
		}
	}
	if err := s.handleMissing(ctx, changes.Missing, len(tracked)); err != nil {
		return err
	}
	// An empty folder can be a wrong mount, so the first scan is the first that
	// finds files. Otherwise fixing the mount would queue the whole library.
	if firstScan && len(files) == 0 {
		s.Log.Printf("first scan: %s is empty, waiting for files before recording the library", s.Cfg.OriginalsDir)
	}
	if firstScan && len(files) > 0 {
		if err := s.Store.SetMeta(model.MetaFirstScanDone, "1"); err != nil {
			return err
		}
		if len(changes.New) > 0 && s.Cfg.Mode == config.ModeAuto {
			s.Log.Printf("first scan: %d existing files recorded as backlog, none queued", len(changes.New))
		}
	}
	if err := s.queueStable(); err != nil {
		return err
	}
	return s.feedBackfill(ctx)
}

func (s *Service) applyRename(r watch.Rename) error {
	if r.From.EncodedSize >= 0 {
		if err := s.moveOutput(OutputPath(r.From.Path), OutputPath(r.To)); err != nil {
			return err
		}
	} else if err := s.removeStub(OutputPath(r.From.Path)); err != nil {
		return err
	}
	if _, err := s.Store.CancelForPath(r.From.Path, ""); err != nil {
		return err
	}
	if err := s.Store.RenamePlacements(OutputPath(r.From.Path), OutputPath(r.To)); err != nil {
		return err
	}
	return s.Store.RenameSource(r.From.Path, r.To)
}

func (s *Service) handleMissing(ctx context.Context, missing []*model.Source, tracked int) error {
	now := s.Now().Unix()
	for _, src := range missing {
		if src.MissingSince == 0 {
			src.MissingSince = now
			if err := s.Store.SetMissing(src.Path, now); err != nil {
				return err
			}
		}
	}

	massive := s.massive(len(missing), tracked)
	s.mu.Lock()
	wasPaused := s.deletePaused
	s.deletePaused = massive
	s.mu.Unlock()
	if massive {
		if !wasPaused {
			s.send(ctx, fmt.Sprintf("Deletions paused: %d of %d sources are missing. Check the originals disk. If this is intended, raise MASS_DELETE_PERCENT.", len(missing), tracked))
		}
		return nil
	}

	grace := int64(s.Cfg.DeleteGrace.Seconds())
	for _, src := range missing {
		if now-src.MissingSince < grace {
			continue
		}
		if err := s.removeSource(src); err != nil {
			s.Log.Printf("remove %s: %v", src.Path, err)
		}
	}
	return nil
}

// massive reports whether a change touches too much of the library at once to
// be ordinary use.
func (s *Service) massive(changed, tracked int) bool {
	return changed >= massDeleteMin && changed*100 > s.Cfg.MassDeletePercent*tracked
}

func (s *Service) removeSource(src *model.Source) error {
	if _, err := s.Store.CancelForPath(src.Path, ""); err != nil {
		return err
	}
	shared, err := s.outputOwnedByAnother(src)
	if err != nil {
		return err
	}
	if !shared {
		if err := s.withdrawPlacements(OutputPath(src.Path)); err != nil {
			return err
		}
		if err := s.removeOutput(OutputPath(src.Path)); err != nil {
			return err
		}
	}
	return s.Store.DeleteSource(src.Path)
}

// outputOwnedByAnother finds a source that shares this one's output and has
// the better claim to it, as x.mkv and x.avi both produce x.mp4. If neither was
// encoded, what is there is a stub, which the other source still needs.
func (s *Service) outputOwnedByAnother(src *model.Source) (bool, error) {
	sources, err := s.Store.Sources()
	if err != nil {
		return false, err
	}
	for _, other := range sources {
		if other.Path != src.Path && OutputPath(other.Path) == OutputPath(src.Path) &&
			(other.EncodedSize >= 0 || src.EncodedSize < 0) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) removeStub(outputPath string) error {
	target := filepath.Join(s.Cfg.LibraryDir, filepath.FromSlash(outputPath))
	if _, err := os.Stat(string(s.Stub)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	isStub, err := s.Stub.Is(target)
	if err != nil || !isStub {
		return err
	}
	return os.Remove(target)
}

func (s *Service) queueStable() error {
	sources, err := s.Store.Sources()
	if err != nil {
		return err
	}
	requested, err := s.Store.RequestedPaths()
	if err != nil {
		return err
	}
	now := s.Now().Unix()
	stable := int64(s.Cfg.StableFor.Seconds())
	owners := map[string]string{}
	for _, src := range sources {
		if src.MissingSince != 0 {
			continue
		}
		output := OutputPath(src.Path)
		if owner, taken := owners[output]; taken {
			// x.mkv and x.avi would overwrite each other's x.mp4.
			s.Log.Printf("skipping %s: its output is already produced by %s", src.Path, owner)
			continue
		}
		owners[output] = src.Path
		if src.Encoded() || now-src.LastChanged < stable {
			continue
		}
		if s.Cfg.Mode == config.ModeManual {
			if src.EncodedSize < 0 {
				if err := s.Stub.Create(filepath.Join(s.Cfg.LibraryDir, filepath.FromSlash(output))); err != nil {
					return err
				}
			}
			// A title is only encoded unasked if it was already live, so an
			// upgrade replaces it, or if a viewer's request is still open.
			if src.EncodedSize < 0 && !requested[src.Path] {
				continue
			}
		} else if src.Backlog {
			continue
		}
		// Manual mode has no backfill, so held changes wait for queue add.
		if src.Backlog && src.EncodedSize >= 0 {
			continue
		}
		if _, err := s.Store.Enqueue(s.newJob(src, model.PriorityNew)); err != nil {
			return err
		}
	}
	return nil
}

// ensureQueued makes sure a current job exists, reviving a cancelled one. A
// failed job stays failed, because replaying a stub is not an operator's retry.
func (s *Service) ensureQueued(src *model.Source) (*model.Job, error) {
	j := s.newJob(src, model.PriorityNew)
	created, err := s.Store.Enqueue(j)
	if err != nil {
		return nil, err
	}
	if created {
		return s.Store.Get(j.ID)
	}
	existing, err := s.Store.Get(j.ID)
	if err != nil {
		return nil, err
	}
	if existing.State == model.StateCancelled {
		if err := s.Store.Retry(j.ID); err != nil {
			return nil, err
		}
		return s.Store.Get(j.ID)
	}
	return existing, nil
}

func (s *Service) newJob(src *model.Source, priority int) *model.Job {
	return &model.Job{
		ID:          job.ComputeID(src.Path, src.Size, src.MtimeNS, s.Profile, src.Revision),
		Path:        src.Path,
		OutputPath:  OutputPath(src.Path),
		SourceSize:  src.Size,
		SourceMtime: src.MtimeNS,
		Revision:    src.Revision,
		Profile:     s.Profile,
		Priority:    priority,
	}
}

// Add queues one tracked source by hand, including one held in the backlog.
func (s *Service) Add(sourcePath string) (string, error) {
	src, err := s.Store.Source(sourcePath)
	if err != nil {
		return "", fmt.Errorf("%s: %w", sourcePath, err)
	}
	if src.Encoded() {
		return "", fmt.Errorf("%s is already encoded, order a re-encode instead", sourcePath)
	}
	j := s.newJob(src, model.PriorityNew)
	existing, err := s.Store.Get(j.ID)
	if err == nil && existing.State != model.StateCancelled {
		return "", fmt.Errorf("job %s is already %s", j.ID, existing.State)
	}
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		return "", err
	}
	queued, err := s.ensureQueued(src)
	if err != nil {
		return "", err
	}
	return queued.ID, nil
}

// ReencodeOptions are the operator's choices for a re-encode order.
type ReencodeOptions struct {
	Reason      string
	ForceEncode bool
	Software    bool
}

// Reencode orders a title encoded again. The current output stays live until
// the new one is verified and published.
func (s *Service) Reencode(sourcePath string, opts ReencodeOptions) (string, error) {
	src, err := s.Store.Source(sourcePath)
	if err != nil {
		return "", fmt.Errorf("%s: %w", sourcePath, err)
	}
	if _, err := os.Stat(filepath.Join(s.Cfg.OriginalsDir, filepath.FromSlash(sourcePath))); err != nil {
		return "", fmt.Errorf("cannot re-encode without the original: %w", err)
	}
	active, err := s.Store.HasActive(sourcePath)
	if err != nil {
		return "", err
	}
	if active {
		return "", fmt.Errorf("%s already has a job in progress", sourcePath)
	}
	if src.Revision, err = s.Store.BumpRevision(sourcePath); err != nil {
		return "", err
	}
	j := s.newJob(src, model.PriorityNew)
	j.Reason = opts.Reason
	j.ForceEncode = opts.ForceEncode
	j.Software = opts.Software
	if _, err := s.Store.Enqueue(j); err != nil {
		return "", err
	}
	return j.ID, nil
}

// WorkerJob builds the job a worker receives, with paths as it sees them.
func (s *Service) WorkerJob(q *model.Job) *job.Job {
	// Without rclone paths the worker transfers through the control API, and
	// the orchestrator picks the files from the job's lease, not these values.
	source, incoming := q.Path, "control-api"
	if s.Cfg.RemoteOriginals != "" {
		source, incoming = s.Cfg.RemoteOriginals+"/"+q.Path, s.Cfg.RemoteIncoming
	}
	return &job.Job{
		ID:          q.ID,
		Source:      source,
		Incoming:    incoming,
		OutputPath:  q.OutputPath,
		Revision:    q.Revision,
		Reason:      q.Reason,
		ForceEncode: q.ForceEncode,
		Software:    q.Software,
		Profile:     s.Cfg.Output,
	}
}

// OutputPath maps a source to its library path.
func OutputPath(sourcePath string) string {
	return strings.TrimSuffix(sourcePath, path.Ext(sourcePath)) + ".mp4"
}

// Title is a short display name for messages.
func Title(sourcePath string) string {
	base := path.Base(sourcePath)
	return strings.TrimSuffix(base, path.Ext(base))
}

func (s *Service) requestRefresh(ctx context.Context) {
	if s.Library == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// One season pack publishes many files, so each one pushes the scan back.
	if s.refreshTimer != nil {
		s.refreshTimer.Stop()
	}
	s.refreshTimer = time.AfterFunc(s.Cfg.LibraryDebounce, func() {
		if err := s.Library.Refresh(context.WithoutCancel(ctx)); err != nil && !errors.Is(err, context.Canceled) {
			s.Log.Printf("library refresh: %v", err)
		}
	})
}

func (s *Service) stopRefreshTimer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshTimer != nil {
		s.refreshTimer.Stop()
	}
}
