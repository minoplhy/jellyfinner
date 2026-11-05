package orchestrator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"jellyfinner/internal/model"
)

func (f *fixture) edgeSetup(quota int64) {
	if err := f.svc.Nodes.Add(&model.Node{ID: "edge1", Name: "edge1", Kind: model.NodeEdge, TokenHash: "x", URL: "https://edge1.example.com", QuotaBytes: quota}); err != nil {
		panic(err)
	}
}

func TestPinOnlyFinishedEncodes(t *testing.T) {
	f := newManual(t)
	f.edgeSetup(1000)
	f.write("a.mkv", "a")
	f.stabilise()
	if err := f.svc.Pin("a.mkv", "edge1"); err == nil {
		t.Error("a stub must never reach a node")
	}
	if err := f.svc.Pin("unknown.mkv", "edge1"); err == nil {
		t.Error("an unknown title must be refused")
	}

	f.play("a.mp4", "item", "alice", "")
	j := f.claim()
	f.upload(j, map[string]string{"a.mp4": "video"})
	f.complete(j)
	f.svc.PublishPending(context.Background())
	if err := f.svc.Pin("a.mkv", "nowhere"); err == nil {
		t.Error("an unknown node must be refused")
	}
	if err := f.svc.Pin("a.mkv", "edge1"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Pin("a.mkv", "edge1"); err == nil {
		t.Error("a duplicate must be refused")
	}
	ps, _ := f.store.NodePlacements("edge1")
	if len(ps) != 1 || ps[0].State != model.PlacementPulling || ps[0].Output != "a.mp4" || ps[0].Size != 5 {
		t.Errorf("placements = %+v", ps)
	}
}

func TestPinRespectsQuota(t *testing.T) {
	f := newFixture(t)
	f.edgeSetup(4)
	f.settled()
	f.write("a.mkv", "a")
	f.finishTitle("a.mkv", "12345")
	if err := f.svc.Pin("a.mkv", "edge1"); err == nil {
		t.Error("over quota must be refused, and nothing evicted")
	}
}

func TestUnpinWarnsWhenRecentlyPlayed(t *testing.T) {
	f := newFixture(t)
	f.edgeSetup(1000)
	f.settled()
	f.write("a.mkv", "a")
	f.finishTitle("a.mkv", "video")
	if err := f.svc.Pin("a.mkv", "edge1"); err != nil {
		t.Fatal(err)
	}
	src, _ := f.store.Source("a.mkv")
	if err := f.store.ActivatePlacement("edge1", src.EncodedSHA256); err != nil {
		t.Fatal(err)
	}
	if err := f.store.TouchPlacement("edge1", src.EncodedSHA256); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.Unpin("a.mkv", "edge1", false); err == nil {
		t.Error("a recently played title needs force")
	}
	if err := f.svc.Unpin("a.mkv", "edge1", true); err != nil {
		t.Fatal(err)
	}
	ps, _ := f.store.NodePlacements("edge1")
	if ps[0].State != model.PlacementDeleting {
		t.Errorf("state = %s", ps[0].State)
	}
	if _, err := f.store.ActivePlacement("a.mp4", "edge1"); err == nil {
		t.Error("a removed title must stop being served at once")
	}
	if err := f.svc.Unpin("a.mkv", "edge1", true); err == nil {
		t.Error("removing twice must report it")
	}
}

func TestReencodeDropsOldCopyAndRepulls(t *testing.T) {
	f := newFixture(t)
	f.edgeSetup(1000)
	f.settled()
	f.write("a.mkv", "a")
	f.finishTitle("a.mkv", "v1")
	if err := f.svc.Pin("a.mkv", "edge1"); err != nil {
		t.Fatal(err)
	}
	old, _ := f.store.Source("a.mkv")
	if err := f.store.ActivatePlacement("edge1", old.EncodedSHA256); err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.Reencode("a.mkv", ReencodeOptions{Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	j := f.claim()
	f.upload(j, map[string]string{"a.mp4": "v2 is new"})
	f.complete(j)
	f.svc.PublishPending(context.Background())

	if _, err := f.store.ActivePlacement("a.mp4", "edge1"); err == nil {
		t.Error("nothing may be served until the new copy has arrived")
	}
	cur, _ := f.store.Source("a.mkv")
	states := map[string]string{}
	ps, _ := f.store.NodePlacements("edge1")
	for _, p := range ps {
		states[p.SHA256] = p.State
	}
	if states[old.EncodedSHA256] != model.PlacementDeleting || states[cur.EncodedSHA256] != model.PlacementPulling {
		t.Errorf("states = %v", states)
	}
}

func TestRenameFollowsPlacement(t *testing.T) {
	f := newFixture(t)
	f.edgeSetup(1000)
	f.settled()
	f.write("old/a.mkv", "a")
	f.finishTitle("old/a.mkv", "video")
	if err := f.svc.Pin("old/a.mkv", "edge1"); err != nil {
		t.Fatal(err)
	}
	f.rename("old/a.mkv", "new/a.mkv")
	f.scan()
	if ps, _ := f.store.OutputPlacements("new/a.mp4"); len(ps) != 1 {
		t.Errorf("placement did not follow: %+v", ps)
	}
}

func TestDeletedSourceWithdrawsPlacement(t *testing.T) {
	f := newFixture(t)
	f.edgeSetup(1000)
	for _, n := range []string{"k1", "k2", "k3", "k4", "k5", "k6", "k7"} {
		f.write(n+".mkv", n)
	}
	f.scan()
	f.write("a.mkv", "a")
	f.finishTitle("a.mkv", "video")
	if err := f.svc.Pin("a.mkv", "edge1"); err != nil {
		t.Fatal(err)
	}
	f.remove("a.mkv")
	f.scan()
	f.advance(2 * time.Hour)
	f.scan()
	ps, _ := f.store.NodePlacements("edge1")
	if len(ps) != 1 || ps[0].State != model.PlacementDeleting {
		t.Errorf("placements = %+v", ps)
	}
}

func (f *fixture) rename(from, to string) {
	f.t.Helper()
	f.mustMkdir(filepath.Dir(filepath.Join(f.cfg.OriginalsDir, to)))
	if err := osRename(filepath.Join(f.cfg.OriginalsDir, from), filepath.Join(f.cfg.OriginalsDir, to)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) mustMkdir(dir string) {
	f.t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) remove(path string) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.cfg.OriginalsDir, path)); err != nil {
		f.t.Fatal(err)
	}
}

var osRename = os.Rename

func TestProbeToleratesTrailingSlash(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	if err := f.svc.Nodes.Add(&model.Node{ID: "edge1", Name: "edge1", Kind: model.NodeEdge, TokenHash: "x", URL: srv.URL + "/"}); err != nil {
		t.Fatal(err)
	}
	f.svc.probeNodes(context.Background())
	st, err := f.store.GetNodeStatus("edge1")
	if err != nil || !st.Healthy {
		t.Errorf("status = %+v, %v", st, err)
	}
}

func TestProbeUnderAPath(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/edge1/health" {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	if err := f.svc.Nodes.Add(&model.Node{ID: "edge1", Name: "edge1", Kind: model.NodeEdge, TokenHash: "x", URL: srv.URL + "/edge1"}); err != nil {
		t.Fatal(err)
	}
	f.svc.probeNodes(context.Background())
	if st, err := f.store.GetNodeStatus("edge1"); err != nil || !st.Healthy {
		t.Errorf("status = %+v, %v", st, err)
	}
}
