package orchestrator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"jellyfinner/internal/config"
	"jellyfinner/internal/model"
)

const webhookToken = "fedcba9876543210fedcba9876543210"

func newManual(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.cfg.Mode = config.ModeManual
	f.cfg.WebhookToken = webhookToken
	if err := f.svc.Stub.Ensure(); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) isStub(output string) bool {
	f.t.Helper()
	ok, err := f.svc.Stub.Is(filepath.Join(f.cfg.LibraryDir, output))
	if err != nil {
		f.t.Fatal(err)
	}
	return ok
}

// play simulates pressing play on a library item.
func (f *fixture) play(output, item, user, session string) {
	f.t.Helper()
	f.library.paths[item] = filepath.Join(f.cfg.LibraryDir, output)
	err := f.svc.HandlePlayback(context.Background(), PlaybackEvent{
		NotificationType: "PlaybackStart", ItemID: item, UserID: user, Username: user, SessionID: session,
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func TestManualCreatesStubsAndQueuesNothing(t *testing.T) {
	f := newManual(t)
	f.write("Movies/A/A.mkv", "a")
	f.write("Movies/B/B.avi", "b")
	f.stabilise()
	if !f.isStub("Movies/A/A.mp4") || !f.isStub("Movies/B/B.mp4") {
		t.Error("stubs missing")
	}
	if jobs := f.jobs(); len(jobs) != 0 {
		t.Errorf("manual mode must not queue on its own: %+v", jobs)
	}
}

func TestManualStubWaitsForStableFile(t *testing.T) {
	f := newManual(t)
	f.write("a.mkv", "partial")
	f.scan()
	if f.isStub("a.mp4") {
		t.Error("no stub until the file is stable")
	}
}

func TestPlayingStubQueuesOnce(t *testing.T) {
	f := newManual(t)
	f.write("a.mkv", "a")
	f.stabilise()

	f.play("a.mp4", "item1", "alice", "s1")
	f.play("a.mp4", "item1", "bob", "s2")
	f.play("a.mp4", "item1", "alice", "s1")

	jobs := f.jobs()
	if len(jobs) != 1 || jobs[0].State != model.StateQueued || jobs[0].Priority != model.PriorityNew {
		t.Fatalf("jobs = %+v", jobs)
	}
	if !slices.Contains(f.library.messages, "s1:Queued. You will be notified when it is ready.") {
		t.Errorf("messages = %v", f.library.messages)
	}
}

func TestPublishReplacesStubAndClearsPlayed(t *testing.T) {
	f := newManual(t)
	f.write("Movies/A/A.mkv", "a")
	f.stabilise()
	f.play("Movies/A/A.mp4", "item1", "alice", "")
	f.play("Movies/A/A.mp4", "item1", "bob", "")

	j := f.claim()
	f.upload(j, map[string]string{"A.mp4": "real video"})
	f.complete(j)
	f.svc.PublishPending(context.Background())

	if f.isStub("Movies/A/A.mp4") {
		t.Fatal("the stub was not replaced")
	}
	if got, _ := f.inLibrary("Movies/A/A.mp4"); got != "real video" {
		t.Errorf("library = %q", got)
	}
	slices.Sort(f.library.cleared)
	if !slices.Equal(f.library.cleared, []string{"item1/alice", "item1/bob"}) {
		t.Errorf("cleared = %v", f.library.cleared)
	}
	if !f.notify.has("(requested by") || !f.notify.has("alice") || !f.notify.has("bob") {
		t.Errorf("notification = %v", f.notify.messages)
	}

	data, err := os.ReadFile(string(f.svc.Stub))
	if err != nil || strings.Contains(string(data), "real video") {
		t.Errorf("publishing must not touch the master: %v", err)
	}

	f.advance(time.Hour)
	f.scan()
	if f.isStub("Movies/A/A.mp4") {
		t.Error("a scan put the stub back over a real encode")
	}
}

func TestPlayingRealEncodeQueuesNothing(t *testing.T) {
	f := newManual(t)
	f.write("a.mkv", "a")
	f.stabilise()
	f.play("a.mp4", "item1", "alice", "")
	j := f.claim()
	f.upload(j, map[string]string{"a.mp4": "video"})
	f.complete(j)
	f.svc.PublishPending(context.Background())

	before := len(f.jobs())
	f.play("a.mp4", "item1", "alice", "")
	if got := len(f.jobs()); got != before {
		t.Errorf("jobs went from %d to %d", before, got)
	}
}

func TestPlaybackIgnoresOtherEvents(t *testing.T) {
	f := newManual(t)
	f.write("a.mkv", "a")
	f.stabilise()
	f.library.paths["item1"] = filepath.Join(f.cfg.LibraryDir, "a.mp4")
	f.library.paths["outside"] = "/somewhere/else/a.mp4"
	f.library.paths["escape"] = filepath.Join(f.cfg.LibraryDir, "../originals/a.mp4")

	for _, e := range []PlaybackEvent{
		{NotificationType: "PlaybackStop", ItemID: "item1"},
		{NotificationType: "PlaybackStart"},
		{NotificationType: "PlaybackStart", ItemID: "outside"},
		{NotificationType: "PlaybackStart", ItemID: "escape"},
	} {
		if err := f.svc.HandlePlayback(context.Background(), e); err != nil {
			t.Errorf("%+v: %v", e, err)
		}
	}
	if jobs := f.jobs(); len(jobs) != 0 {
		t.Errorf("jobs = %+v", jobs)
	}
}

func TestPlaybackMapsContainerPaths(t *testing.T) {
	f := newManual(t)
	f.cfg.JellyfinLibraryDir = "/media"
	f.write("Movies/A/A.mkv", "a")
	f.stabilise()
	f.library.paths["item1"] = "/media/Movies/A/A.mp4"
	if err := f.svc.HandlePlayback(context.Background(), PlaybackEvent{NotificationType: "PlaybackStart", ItemID: "item1"}); err != nil {
		t.Fatal(err)
	}
	if len(f.jobs()) != 1 {
		t.Error("a path under JELLYFIN_LIBRARY_DIR must resolve")
	}
}

func TestPlayingFailedTitleDoesNotRetry(t *testing.T) {
	f := newManual(t)
	f.write("a.mkv", "a")
	f.stabilise()
	f.play("a.mp4", "item1", "alice", "s1")
	for range f.cfg.MaxAttempts {
		j := f.claim()
		if _, err := f.store.Fail(j.ID, j.LeaseID, "boom", "", f.cfg.MaxAttempts); err != nil {
			t.Fatal(err)
		}
	}
	f.play("a.mp4", "item1", "alice", "s1")
	if f.queued("a.mkv") != nil {
		t.Error("replaying a stub must not retry a failed encode")
	}
	if !slices.ContainsFunc(f.library.messages, func(m string) bool { return strings.Contains(m, "could not be prepared") }) {
		t.Errorf("messages = %v", f.library.messages)
	}
}

func TestChangedSourceKeepsOpenRequest(t *testing.T) {
	f := newManual(t)
	f.write("a.mkv", "one")
	f.stabilise()
	f.play("a.mp4", "item1", "alice", "")
	running := f.claim()

	f.write("a.mkv", "two two")
	f.stabilise()
	if j, _ := f.store.Get(running.ID); j.State != model.StateCancelled {
		t.Errorf("old job = %+v", j)
	}
	if f.queued("a.mkv") == nil {
		t.Error("the viewer's request must survive a source upgrade")
	}
}

func TestUpgradedLiveTitleRequeues(t *testing.T) {
	f := newManual(t)
	f.write("a.mkv", "one")
	f.stabilise()
	f.play("a.mp4", "item1", "alice", "")
	j := f.claim()
	f.upload(j, map[string]string{"a.mp4": "v1"})
	f.complete(j)
	f.svc.PublishPending(context.Background())

	f.write("a.mkv", "two two")
	f.stabilise()
	if f.queued("a.mkv") == nil {
		t.Error("a replaced source of a live title must requeue")
	}
	if got, _ := f.inLibrary("a.mp4"); got != "v1" {
		t.Errorf("the old encode must stay live: %q", got)
	}
}

func TestRenameMovesStub(t *testing.T) {
	f := newManual(t)
	f.write("old/a.mkv", "one")
	f.stabilise()
	if err := os.MkdirAll(filepath.Join(f.cfg.OriginalsDir, "new"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(f.cfg.OriginalsDir, "old/a.mkv"), filepath.Join(f.cfg.OriginalsDir, "new/a.mkv")); err != nil {
		t.Fatal(err)
	}
	f.stabilise()
	if f.isStub("old/a.mp4") {
		t.Error("old stub remains")
	}
	if !f.isStub("new/a.mp4") {
		t.Error("new stub missing")
	}
}

func TestDeleteRemovesStub(t *testing.T) {
	f := newManual(t)
	for _, name := range []string{"k1", "k2", "k3", "k4", "k5", "k6"} {
		f.write(name+".mkv", name)
	}
	f.write("a.mkv", "a")
	f.stabilise()
	if err := os.Remove(filepath.Join(f.cfg.OriginalsDir, "a.mkv")); err != nil {
		t.Fatal(err)
	}
	f.scan()
	f.advance(2 * time.Hour)
	f.scan()
	if f.isStub("a.mp4") {
		t.Error("stub not removed")
	}
	if !f.isStub("k1.mp4") {
		t.Error("other stubs must stay")
	}
}

func TestSwitchManualToAutoHoldsStubsAsBacklog(t *testing.T) {
	f := newManual(t)
	f.write("a.mkv", "a")
	f.write("b.mkv", "b")
	f.stabilise()
	f.play("b.mp4", "item", "alice", "")
	j := f.claim()
	f.upload(j, map[string]string{"b.mp4": "video"})
	f.complete(j)
	f.svc.PublishPending(context.Background())

	n, err := f.store.SwitchMode("auto")
	if err != nil || n != 1 {
		t.Fatalf("SwitchMode = %d, %v", n, err)
	}
	f.cfg.Mode = config.ModeAuto
	f.advance(time.Hour)
	f.scan()
	if f.queued("a.mkv") != nil {
		t.Error("switching must not start encoding the library")
	}
	if !f.isStub("a.mp4") {
		t.Error("the title must not vanish")
	}
	if got, _ := f.inLibrary("b.mp4"); got != "video" {
		t.Errorf("finished titles stay: %q", got)
	}
}

func TestSwitchAutoToManualCancelsQueuedAndCreatesStubs(t *testing.T) {
	f := newFixture(t)
	f.settled()
	f.write("a.mkv", "a")
	f.write("b.mkv", "b")
	f.stabilise()
	running := f.claim()

	f.cfg.WebhookToken = webhookToken
	if n, err := f.store.SwitchMode("manual"); err != nil || n != 1 {
		t.Fatalf("SwitchMode = %d, %v", n, err)
	}
	f.cfg.Mode = config.ModeManual
	if err := f.svc.Stub.Ensure(); err != nil {
		t.Fatal(err)
	}
	if j, _ := f.store.Get(running.ID); j.State != model.StateRunning {
		t.Errorf("the running job finishes normally: %+v", j)
	}
	f.scan()
	stubbed := "a.mp4"
	if running.Path == "a.mkv" {
		stubbed = "b.mp4"
	}
	if !f.isStub(stubbed) {
		t.Errorf("%s needs a stub", stubbed)
	}

	// The cancelled job shares its ID with a new request, so it must revive.
	f.play(stubbed, "item", "alice", "")
	if f.queued(strings.TrimSuffix(stubbed, ".mp4")+".mkv") == nil {
		t.Error("pressing play must revive a cancelled job")
	}
}

func TestWebhookAuth(t *testing.T) {
	f := newManual(t)
	f.write("a.mkv", "a")
	f.stabilise()
	f.library.paths["item1"] = filepath.Join(f.cfg.LibraryDir, "a.mp4")
	server := httptest.NewServer(f.svc.WebhookHandler())
	defer server.Close()

	body := `{"NotificationType":"PlaybackStart","ItemId":"item1","UserId":"u"}`
	post := func(token, body string) int {
		req, err := http.NewRequest(http.MethodPost, server.URL+"/webhook/jellyfin", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("X-Webhook-Token", token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := post("", body); got != http.StatusUnauthorized {
		t.Errorf("no token: %d", got)
	}
	if got := post("wrong", body); got != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", got)
	}
	if len(f.jobs()) != 0 {
		t.Fatal("a refused webhook queued work")
	}
	if got := post(webhookToken, "not json"); got != http.StatusBadRequest {
		t.Errorf("bad body: %d", got)
	}
	if got := post(webhookToken, body); got != http.StatusNoContent {
		t.Errorf("good request: %d", got)
	}
	if len(f.jobs()) != 1 {
		t.Error("the job was not queued")
	}
}
