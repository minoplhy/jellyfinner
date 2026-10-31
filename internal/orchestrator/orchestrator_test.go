package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyfinner/internal/config"
	"jellyfinner/internal/pipeline"
	"jellyfinner/internal/queue"
)

type fakeNotifier struct {
	mu       sync.Mutex
	messages []string
}

func (f *fakeNotifier) Send(_ context.Context, m string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, m)
	return nil
}

func (f *fakeNotifier) has(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.messages {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

type fakeLibrary struct {
	mu       sync.Mutex
	calls    int
	paths    map[string]string
	cleared  []string
	messages []string
}

func (f *fakeLibrary) Refresh(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return nil
}

func (f *fakeLibrary) ItemPath(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.paths[id]
	if !ok {
		return "", fmt.Errorf("no item %s", id)
	}
	return p, nil
}

func (f *fakeLibrary) ClearPlayed(_ context.Context, item, user string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, item+"/"+user)
	return nil
}

func (f *fakeLibrary) SendMessage(_ context.Context, session, _, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, session+":"+text)
	return nil
}

func (f *fakeLibrary) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fixture struct {
	t       *testing.T
	svc     *Service
	store   *queue.Store
	cfg     *config.Config
	notify  *fakeNotifier
	library *fakeLibrary
	now     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{
		Mode:              config.ModeAuto,
		OriginalsDir:      filepath.Join(root, "originals"),
		LibraryDir:        filepath.Join(root, "library"),
		IncomingDir:       filepath.Join(root, "incoming"),
		RemoteOriginals:   "origin:/originals",
		RemoteIncoming:    "origin:/incoming",
		StableFor:         5 * time.Minute,
		DeleteGrace:       time.Hour,
		ScanInterval:      time.Hour,
		MassDeletePercent: 20,
		MaxAttempts:       2,
		LeaseFor:          time.Minute,
		LibraryDebounce:   10 * time.Millisecond,
	}
	for _, d := range []string{cfg.OriginalsDir, cfg.LibraryDir, cfg.IncomingDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	store, err := queue.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	store.RetryDelay = 0

	f := &fixture{t: t, store: store, cfg: cfg, notify: &fakeNotifier{}, library: &fakeLibrary{paths: map[string]string{}}, now: time.Unix(2000000, 0)}
	store.Now = func() time.Time { return f.now }
	f.svc = New(cfg, store, f.notify, f.library, log.New(io.Discard, "", 0))
	return f
}

func (f *fixture) advance(d time.Duration) { f.now = f.now.Add(d) }

func (f *fixture) write(path, content string) {
	f.t.Helper()
	full := filepath.Join(f.cfg.OriginalsDir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o640); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) scan() {
	f.t.Helper()
	if err := f.svc.ScanOnce(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

// settled runs the first scan, so later files count as new arrivals.
func (f *fixture) settled() {
	f.t.Helper()
	f.write("seed/seed.mkv", "seed")
	f.scan()
}

// stabilise lets the stability wait pass for files written since the last scan.
// The wait starts when a scan first sees a file.
func (f *fixture) stabilise() {
	f.t.Helper()
	f.scan()
	f.advance(10 * time.Minute)
	f.scan()
}

func (f *fixture) jobs() []*queue.Job {
	f.t.Helper()
	jobs, err := f.store.List(100)
	if err != nil {
		f.t.Fatal(err)
	}
	return jobs
}

func (f *fixture) queued(path string) *queue.Job {
	f.t.Helper()
	for _, j := range f.jobs() {
		if j.Path == path && j.State == queue.StateQueued {
			return j
		}
	}
	return nil
}

// upload simulates a worker pushing a finished job.
func (f *fixture) upload(j *queue.Job, files map[string]string) string {
	f.t.Helper()
	dir := filepath.Join(f.cfg.IncomingDir, j.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		f.t.Fatal(err)
	}
	m := pipeline.Manifest{JobID: j.ID, OutputPath: j.OutputPath}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o640); err != nil {
			f.t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(content))
		m.Files = append(m.Files, pipeline.File{Name: name, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])})
	}
	data, err := json.Marshal(m)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pipeline.ManifestName), data, 0o640); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func (f *fixture) claim() *queue.Job {
	f.t.Helper()
	j, err := f.store.Claim("worker1", time.Minute)
	if err != nil {
		f.t.Fatal(err)
	}
	if j == nil {
		f.t.Fatal("nothing to claim")
	}
	return j
}

func (f *fixture) complete(j *queue.Job) {
	f.t.Helper()
	if err := f.store.Complete(j.ID, j.LeaseID, "{}", ""); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) inLibrary(path string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(f.cfg.LibraryDir, path))
	return string(data), err == nil
}

func (f *fixture) finishTitle(source, content string) {
	f.t.Helper()
	f.stabilise()
	j := f.claim()
	f.upload(j, map[string]string{filepath.Base(j.OutputPath): content})
	f.complete(j)
	f.svc.PublishPending(context.Background())
	if _, ok := f.inLibrary(strings.TrimSuffix(source, filepath.Ext(source)) + ".mp4"); !ok {
		f.t.Fatalf("%s did not publish", source)
	}
}

func TestFirstScanIsBacklogNotQueued(t *testing.T) {
	f := newFixture(t)
	f.write("Movies/A/A.mkv", "a")
	f.write("Movies/B/B.mkv", "b")
	f.scan()
	f.advance(time.Hour)
	f.scan()
	if jobs := f.jobs(); len(jobs) != 0 {
		t.Errorf("the existing library must not queue itself: %+v", jobs)
	}
	src, err := f.store.Source("Movies/A/A.mkv")
	if err != nil || !src.Backlog {
		t.Errorf("source = %+v, %v", src, err)
	}
}

func TestNewFileWaitsUntilStable(t *testing.T) {
	f := newFixture(t)
	f.settled()
	f.write("Movies/N/N.mkv", "part")
	f.scan()
	if f.queued("Movies/N/N.mkv") != nil {
		t.Fatal("queued before it was stable")
	}

	f.advance(4 * time.Minute)
	f.write("Movies/N/N.mkv", "part-more")
	f.scan()
	f.advance(4 * time.Minute)
	f.scan()
	if f.queued("Movies/N/N.mkv") != nil {
		t.Fatal("a change must restart the stability wait")
	}

	f.advance(2 * time.Minute)
	f.scan()
	j := f.queued("Movies/N/N.mkv")
	if j == nil {
		t.Fatal("not queued once stable")
	}
	if j.OutputPath != "Movies/N/N.mp4" || j.Priority != queue.PriorityNew {
		t.Errorf("job = %+v", j)
	}
	if got := f.svc.WorkerJob(j); got.Source != "origin:/originals/Movies/N/N.mkv" || got.Incoming != "origin:/incoming" {
		t.Errorf("worker job = %+v", got)
	}

	f.scan()
	if n := len(f.jobs()); n != 1 {
		t.Errorf("scans must not duplicate jobs: %d", n)
	}
}

func TestFileChangedMidJobCancelsAndRequeues(t *testing.T) {
	f := newFixture(t)
	f.settled()
	f.write("a.mkv", "one")
	f.stabilise()
	running := f.claim()

	f.write("a.mkv", "two two")
	f.scan()
	if j, _ := f.store.Get(running.ID); j.State != queue.StateCancelled {
		t.Errorf("state = %s", j.State)
	}
	f.stabilise()
	if j := f.queued("a.mkv"); j == nil || j.ID == running.ID {
		t.Errorf("replacement job = %+v", j)
	}
}

func TestFailedJobIsNotRequeuedByScans(t *testing.T) {
	f := newFixture(t)
	f.settled()
	f.write("a.mkv", "one")
	f.stabilise()
	for range f.cfg.MaxAttempts {
		j := f.claim()
		if _, err := f.store.Fail(j.ID, j.LeaseID, "boom", "", f.cfg.MaxAttempts); err != nil {
			t.Fatal(err)
		}
	}
	f.advance(time.Hour)
	f.scan()
	if f.queued("a.mkv") != nil {
		t.Error("a failed job must wait for an operator")
	}
}

func TestPublishMovesFilesIntoLibrary(t *testing.T) {
	f := newFixture(t)
	f.settled()
	f.write("Movies/A/A.mkv", "src")
	f.stabilise()
	j := f.claim()
	dir := f.upload(j, map[string]string{"A.mp4": "video", "A.tha.srt": "subs"})
	f.complete(j)
	f.svc.PublishPending(context.Background())

	if got, ok := f.inLibrary("Movies/A/A.mp4"); !ok || got != "video" {
		t.Errorf("video = %q, %v", got, ok)
	}
	if _, ok := f.inLibrary("Movies/A/A.tha.srt"); !ok {
		t.Error("sidecar missing")
	}
	if _, ok := f.inLibrary(j.ID); ok {
		t.Error("nothing but the title's files belongs in the library")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("incoming must be cleared")
	}
	info, err := os.Stat(filepath.Join(f.cfg.LibraryDir, "Movies/A/A.mp4"))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("Jellyfin must be able to read it: %v, %v", info.Mode(), err)
	}
	if got, _ := f.store.Get(j.ID); got.State != queue.StateDone {
		t.Errorf("state = %s", got.State)
	}
	if !f.notify.has("Ready to watch: A") {
		t.Error("no ready notification")
	}
	src, _ := f.store.Source("Movies/A/A.mkv")
	if !src.Encoded() {
		t.Errorf("source = %+v", src)
	}

	f.advance(time.Hour)
	f.scan()
	if f.queued("Movies/A/A.mkv") != nil {
		t.Error("a published title must not requeue")
	}

	deadline := time.Now().Add(2 * time.Second)
	for f.library.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.library.count() != 1 {
		t.Errorf("library refreshes = %d", f.library.count())
	}
}

func TestLibraryRefreshIsDebounced(t *testing.T) {
	f := newFixture(t)
	f.cfg.LibraryDebounce = 50 * time.Millisecond
	for range 5 {
		f.svc.requestRefresh(context.Background())
	}
	time.Sleep(200 * time.Millisecond)
	if n := f.library.count(); n != 1 {
		t.Errorf("refreshes = %d, want 1", n)
	}
}

func TestPublishRejectsBadUploads(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(f *fixture, j *queue.Job, dir string)
	}{
		{"corrupt video", func(f *fixture, _ *queue.Job, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "A.mp4"), []byte("VIDEO"), 0o640); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"truncated video", func(f *fixture, _ *queue.Job, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "A.mp4"), []byte("vid"), 0o640); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"missing manifest", func(f *fixture, _ *queue.Job, dir string) {
			if err := os.Remove(filepath.Join(dir, pipeline.ManifestName)); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"manifest for another job", func(f *fixture, _ *queue.Job, dir string) {
			rewriteManifest(f, dir, func(m *pipeline.Manifest) { m.JobID = "other" })
		}},
		{"path traversal", func(f *fixture, _ *queue.Job, dir string) {
			rewriteManifest(f, dir, func(m *pipeline.Manifest) { m.Files[0].Name = "../../evil.mp4" })
		}},
		{"video not listed", func(f *fixture, _ *queue.Job, dir string) {
			rewriteManifest(f, dir, func(m *pipeline.Manifest) { m.Files = nil })
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.settled()
			f.write("Movies/A/A.mkv", "src")
			f.stabilise()
			j := f.claim()
			dir := f.upload(j, map[string]string{"A.mp4": "video"})
			tt.mutate(f, j, dir)
			f.complete(j)
			f.svc.PublishPending(context.Background())

			if _, ok := f.inLibrary("Movies/A/A.mp4"); ok {
				t.Error("a bad upload reached the library")
			}
			got, _ := f.store.Get(j.ID)
			if got.State != queue.StateQueued || !strings.Contains(got.Error, "publish") {
				t.Errorf("job = %+v", got)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Error("the rejected upload must be cleared before a retry")
			}
		})
	}
}

func rewriteManifest(f *fixture, dir string, mutate func(*pipeline.Manifest)) {
	f.t.Helper()
	file := filepath.Join(dir, pipeline.ManifestName)
	data, err := os.ReadFile(file)
	if err != nil {
		f.t.Fatal(err)
	}
	var m pipeline.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		f.t.Fatal(err)
	}
	mutate(&m)
	if data, err = json.Marshal(m); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o640); err != nil {
		f.t.Fatal(err)
	}
}

func TestPublishFailureKeepsLiveEncode(t *testing.T) {
	f := newFixture(t)
	f.settled()
	f.write("a.mkv", "one")
	f.finishTitle("a.mkv", "good")

	id, err := f.svc.Reencode("a.mkv", ReencodeOptions{Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	j := f.claim()
	if j.ID != id {
		t.Fatalf("claimed %s, want %s", j.ID, id)
	}
	dir := f.upload(j, map[string]string{"a.mp4": "new"})
	if err := os.WriteFile(filepath.Join(dir, "a.mp4"), []byte("bad"), 0o640); err != nil {
		t.Fatal(err)
	}
	f.complete(j)
	f.svc.PublishPending(context.Background())
	if got, _ := f.inLibrary("a.mp4"); got != "good" {
		t.Errorf("library = %q, the old encode must stay", got)
	}
}

func TestPublishSurvivesRestart(t *testing.T) {
	f := newFixture(t)
	f.settled()
	f.write("a.mkv", "one")
	f.stabilise()
	j := f.claim()
	f.upload(j, map[string]string{"a.mp4": "video"})
	f.complete(j)

	restarted := New(f.cfg, f.store, f.notify, f.library, log.New(io.Discard, "", 0))
	restarted.PublishPending(context.Background())
	if _, ok := f.inLibrary("a.mp4"); !ok {
		t.Error("a publish left by a restart must complete")
	}
}

func TestReencodeRemovesDroppedSidecars(t *testing.T) {
	f := newFixture(t)
	f.settled()
	f.write("a.mkv", "one")
	f.stabilise()
	j := f.claim()
	f.upload(j, map[string]string{"a.mp4": "v1", "a.tha.srt": "s", "a.eng.srt": "s", "a.eng.ass": "s"})
	f.complete(j)
	f.svc.PublishPending(context.Background())
	if err := os.WriteFile(filepath.Join(f.cfg.LibraryDir, "a.5.tha.nfo"), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(f.cfg.LibraryDir, "a 2.tha.srt")
	if err := os.WriteFile(other, nil, 0o640); err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.Reencode("a.mkv", ReencodeOptions{Reason: "subs"}); err != nil {
		t.Fatal(err)
	}
	j = f.claim()
	f.upload(j, map[string]string{"a.mp4": "v2", "a.tha.srt": "s2"})
	f.complete(j)
	f.svc.PublishPending(context.Background())

	if got, _ := f.inLibrary("a.mp4"); got != "v2" {
		t.Errorf("video = %q", got)
	}
	if _, ok := f.inLibrary("a.eng.srt"); ok {
		t.Error("a dropped sidecar must be removed")
	}
	if _, ok := f.inLibrary("a.eng.ass"); ok {
		t.Error("a dropped ASS sidecar must be removed")
	}
	if got, ok := f.inLibrary("a.tha.srt"); !ok || got != "s2" {
		t.Errorf("kept sidecar = %q, %v", got, ok)
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("another title's sidecar was touched")
	}
}

func TestReencodeRules(t *testing.T) {
	f := newFixture(t)
	f.settled()
	f.write("a.mkv", "one")
	f.finishTitle("a.mkv", "v1")

	id, err := f.svc.Reencode("a.mkv", ReencodeOptions{Reason: "audio", ForceEncode: true, Software: true})
	if err != nil {
		t.Fatal(err)
	}
	j, err := f.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if j.Reason != "audio" || !j.ForceEncode || !j.Software || j.Priority != queue.PriorityNew || j.Revision != 1 {
		t.Errorf("job = %+v", j)
	}
	if got, _ := f.inLibrary("a.mp4"); got != "v1" {
		t.Error("the live encode must stay until the new one publishes")
	}
	if _, err := f.svc.Reencode("a.mkv", ReencodeOptions{Reason: "again"}); err == nil {
		t.Error("a second active order must be refused")
	}
	if err := f.store.Cancel(id); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(f.cfg.OriginalsDir, "a.mkv")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Reencode("a.mkv", ReencodeOptions{Reason: "x"}); err == nil {
		t.Error("a missing original must be refused")
	}
	if _, err := f.svc.Reencode("unknown.mkv", ReencodeOptions{Reason: "x"}); err == nil {
		t.Error("an unknown title must be refused")
	}
}

func TestAddPullsFromBacklog(t *testing.T) {
	f := newFixture(t)
	f.write("old.mkv", "x")
	f.scan()
	if _, err := f.svc.Add("old.mkv"); err != nil {
		t.Fatal(err)
	}
	if f.queued("old.mkv") == nil {
		t.Error("not queued")
	}
	if _, err := f.svc.Add("old.mkv"); err == nil {
		t.Error("a duplicate must be refused")
	}
}

func TestRenameMovesOutputWithoutEncoding(t *testing.T) {
	f := newFixture(t)
	f.settled()
	f.write("old/a.mkv", "one")
	f.stabilise()
	j := f.claim()
	f.upload(j, map[string]string{"a.mp4": "video", "a.tha.srt": "subs"})
	f.complete(j)
	f.svc.PublishPending(context.Background())

	if err := os.MkdirAll(filepath.Join(f.cfg.OriginalsDir, "new"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(f.cfg.OriginalsDir, "old/a.mkv"), filepath.Join(f.cfg.OriginalsDir, "new/b.mkv")); err != nil {
		t.Fatal(err)
	}
	f.advance(time.Hour)
	f.scan()

	if _, ok := f.inLibrary("new/b.mp4"); !ok {
		t.Error("output did not move")
	}
	if _, ok := f.inLibrary("new/b.tha.srt"); !ok {
		t.Error("sidecar did not move")
	}
	if _, ok := f.inLibrary("old/a.mp4"); ok {
		t.Error("old output remains")
	}
	if f.queued("new/b.mkv") != nil {
		t.Error("a rename must not re-encode")
	}
}

func TestDeleteWaitsForGrace(t *testing.T) {
	f := newFixture(t)
	for i := range 10 {
		f.write(fmt.Sprintf("keep%d.mkv", i), fmt.Sprint(i))
	}
	f.scan()
	f.write("a.mkv", "one")
	f.finishTitle("a.mkv", "video")

	if err := os.Remove(filepath.Join(f.cfg.OriginalsDir, "a.mkv")); err != nil {
		t.Fatal(err)
	}
	f.scan()
	f.advance(30 * time.Minute)
	f.scan()
	if _, ok := f.inLibrary("a.mp4"); !ok {
		t.Fatal("removed inside the grace period")
	}

	f.advance(31 * time.Minute)
	f.scan()
	if _, ok := f.inLibrary("a.mp4"); ok {
		t.Error("output not removed after the grace period")
	}
	if _, err := f.store.Source("a.mkv"); err == nil {
		t.Error("source still tracked")
	}
}

func TestReturnedFileIsNotDeleted(t *testing.T) {
	f := newFixture(t)
	for i := range 10 {
		f.write(fmt.Sprintf("keep%d.mkv", i), fmt.Sprint(i))
	}
	f.scan()
	f.write("a.mkv", "one")
	f.finishTitle("a.mkv", "video")

	full := filepath.Join(f.cfg.OriginalsDir, "a.mkv")
	aside := filepath.Join(f.cfg.OriginalsDir, "a.mkv.moved")
	if err := os.Rename(full, aside); err != nil {
		t.Fatal(err)
	}
	f.scan()
	if err := os.Rename(aside, full); err != nil {
		t.Fatal(err)
	}
	f.advance(2 * time.Hour)
	f.scan()
	if _, ok := f.inLibrary("a.mp4"); !ok {
		t.Error("a file that came back must keep its output")
	}
}

func TestEmptyOriginalsDeletesNothing(t *testing.T) {
	f := newFixture(t)
	f.settled()
	f.write("a.mkv", "one")
	f.finishTitle("a.mkv", "video")

	// An unmounted disk leaves an empty directory behind.
	for _, name := range []string{"a.mkv", "seed"} {
		if err := os.RemoveAll(filepath.Join(f.cfg.OriginalsDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	f.advance(48 * time.Hour)
	if err := f.svc.ScanOnce(context.Background()); err == nil {
		t.Error("an empty tree with tracked sources must fail the scan")
	}
	if _, ok := f.inLibrary("a.mp4"); !ok {
		t.Error("output deleted")
	}
}

func TestMissingRootDeletesNothing(t *testing.T) {
	f := newFixture(t)
	f.settled()
	f.write("a.mkv", "one")
	f.finishTitle("a.mkv", "video")

	if err := os.RemoveAll(f.cfg.OriginalsDir); err != nil {
		t.Fatal(err)
	}
	f.advance(48 * time.Hour)
	f.svc.runScan(context.Background())
	if !f.notify.has("scan failing") {
		t.Error("operator not told")
	}
	if _, ok := f.inLibrary("a.mp4"); !ok {
		t.Error("output deleted")
	}
}

func TestMassMissingPausesDeletion(t *testing.T) {
	f := newFixture(t)
	f.settled()
	var names []string
	for i := range 8 {
		name := fmt.Sprintf("t%d.mkv", i)
		names = append(names, name)
		f.write(name, fmt.Sprint(i))
	}
	f.stabilise()
	for range names {
		j := f.claim()
		f.upload(j, map[string]string{filepath.Base(j.OutputPath): "v"})
		f.complete(j)
	}
	f.svc.PublishPending(context.Background())

	for _, name := range names {
		if err := os.Remove(filepath.Join(f.cfg.OriginalsDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	f.scan()
	f.advance(48 * time.Hour)
	f.scan()
	if !f.notify.has("Deletions paused") {
		t.Error("operator not told")
	}
	if _, ok := f.inLibrary("t0.mp4"); !ok {
		t.Error("a mass disappearance must not delete outputs")
	}
}

func TestOutputCollisionSkipsSecondSource(t *testing.T) {
	f := newFixture(t)
	f.settled()
	f.write("a.avi", "avi")
	f.write("a.mkv", "mkv")
	f.stabilise()
	if f.queued("a.avi") == nil {
		t.Error("the first source should be queued")
	}
	if f.queued("a.mkv") != nil {
		t.Error("two sources must not write the same output")
	}
}

func TestLeaseExpiryNotifiesFinalFailure(t *testing.T) {
	f := newFixture(t)
	f.settled()
	f.write("a.mkv", "one")
	f.stabilise()
	for range f.cfg.MaxAttempts {
		f.claim()
		f.advance(2 * time.Minute)
		f.svc.reap(context.Background())
	}
	if !f.notify.has("Encode failed: a") {
		t.Error("no failure notification")
	}
}
