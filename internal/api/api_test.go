package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jellyfinner/internal/config"
	"jellyfinner/internal/control"
	"jellyfinner/internal/job"
	"jellyfinner/internal/queue"
)

const token = "0123456789abcdef0123456789abcdef"

type testNode struct {
	Name  string
	Token string
	Allow []netip.Prefix
	Edge  bool
}

type fixture struct {
	store     *queue.Store
	cfg       *config.Config
	server    *httptest.Server
	client    *control.Client
	completed int
	failed    []*queue.Job
	nodes     []testNode

	edgeFailed []string
}

func newFixture(t *testing.T, nodes []testNode) *fixture {
	t.Helper()
	store, err := queue.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	store.RetryDelay = 0

	for _, n := range nodes {
		node := &queue.Node{Name: n.Name, Kind: queue.NodeWorker, TokenHash: queue.HashToken(n.Token), Allow: n.Allow}
		if n.Edge {
			node.Kind = queue.NodeEdge
		}
		if err := store.AddNode(node); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{LeaseFor: time.Minute, MaxAttempts: 2}
	f := &fixture{store: store, nodes: nodes, cfg: cfg}
	srv := New(cfg, store, Hooks{
		WorkerJob: func(j *queue.Job) *job.Job {
			return &job.Job{ID: j.ID, Source: "origin:/" + j.Path, Incoming: "origin:/incoming", OutputPath: j.OutputPath}
		},
		Completed: func() { f.completed++ },
		Failed:    func(j *queue.Job) { f.failed = append(f.failed, j) },
		EdgeFailed: func(node, sha256, reason string) {
			f.edgeFailed = append(f.edgeFailed, node+":"+reason)
		},
	}, log.New(io.Discard, "", 0))
	f.server = httptest.NewServer(srv.Handler())
	t.Cleanup(f.server.Close)
	if f.client, err = control.NewClient(f.server.URL, token); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) enqueue(t *testing.T, id string) {
	t.Helper()
	if _, err := f.store.Enqueue(&queue.Job{ID: id, Path: id + ".mkv", OutputPath: id + ".mp4", Priority: queue.PriorityNew}); err != nil {
		t.Fatal(err)
	}
}

func allNodes() []testNode { return []testNode{{Name: "worker1", Token: token}} }

func TestClaimHeartbeatFinish(t *testing.T) {
	f := newFixture(t, allNodes())
	ctx := context.Background()

	if claim, err := f.client.Claim(ctx); err != nil || claim != nil {
		t.Fatalf("empty Claim = %+v, %v", claim, err)
	}
	f.enqueue(t, "a")
	claim, err := f.client.Claim(ctx)
	if err != nil || claim == nil || claim.Job.Source != "origin:/a.mkv" || claim.Lease == "" {
		t.Fatalf("Claim = %+v, %v", claim, err)
	}
	if j, _ := f.store.Get("a"); j.Node != "worker1" {
		t.Errorf("node = %q", j.Node)
	}

	stop, err := f.client.Heartbeat(ctx, "a", control.HeartbeatRequest{Lease: claim.Lease, Stage: "encode", Fraction: 0.25})
	if err != nil || stop {
		t.Fatalf("Heartbeat = %v, %v", stop, err)
	}
	if err := f.client.Finish(ctx, "a", control.FinishRequest{Lease: claim.Lease, Manifest: json.RawMessage(`{"job_id":"a"}`), Log: "ok"}); err != nil {
		t.Fatal(err)
	}
	j, _ := f.store.Get("a")
	if j.State != queue.StatePublishing || j.Log != "ok" || f.completed != 1 {
		t.Errorf("job = %+v, completed = %d", j, f.completed)
	}
}

func TestCompleteNeedsManifest(t *testing.T) {
	f := newFixture(t, allNodes())
	f.enqueue(t, "a")
	claim, err := f.client.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.client.Finish(context.Background(), "a", control.FinishRequest{Lease: claim.Lease}); err == nil {
		t.Error("a result without a manifest must be refused")
	}
}

func TestFinishFailureRetriesThenReports(t *testing.T) {
	f := newFixture(t, allNodes())
	ctx := context.Background()
	f.enqueue(t, "a")
	for attempt := 1; attempt <= 2; attempt++ {
		claim, err := f.client.Claim(ctx)
		if err != nil || claim == nil {
			t.Fatalf("attempt %d: %+v, %v", attempt, claim, err)
		}
		if err := f.client.Finish(ctx, "a", control.FinishRequest{Lease: claim.Lease, Error: "boom", Log: "tail"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.failed) != 1 || f.failed[0].Error != "boom" {
		t.Errorf("failed = %+v", f.failed)
	}
}

func TestStaleLeaseGetsConflict(t *testing.T) {
	f := newFixture(t, allNodes())
	ctx := context.Background()
	f.enqueue(t, "a")
	if _, err := f.client.Claim(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Heartbeat(ctx, "a", control.HeartbeatRequest{Lease: "wrong"}); !errors.Is(err, control.ErrLeaseLost) {
		t.Errorf("Heartbeat: %v", err)
	}
	err := f.client.Finish(ctx, "a", control.FinishRequest{Lease: "wrong", Manifest: json.RawMessage(`{}`)})
	if !errors.Is(err, control.ErrLeaseLost) {
		t.Errorf("Finish: %v", err)
	}
}

func TestCancelReachesHeartbeat(t *testing.T) {
	f := newFixture(t, allNodes())
	ctx := context.Background()
	f.enqueue(t, "a")
	claim, err := f.client.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Cancel("a"); err != nil {
		t.Fatal(err)
	}
	stop, err := f.client.Heartbeat(ctx, "a", control.HeartbeatRequest{Lease: claim.Lease})
	if err != nil || !stop {
		t.Errorf("Heartbeat = %v, %v", stop, err)
	}
}

func post(t *testing.T, url, auth string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/v1/jobs/claim", nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestAuthentication(t *testing.T) {
	f := newFixture(t, allNodes())
	if got := post(t, f.server.URL, ""); got != http.StatusUnauthorized {
		t.Errorf("no token: %d", got)
	}
	if got := post(t, f.server.URL, "Bearer "+strings.Repeat("x", 32)); got != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", got)
	}
	if got := post(t, f.server.URL, "Bearer "); got != http.StatusUnauthorized {
		t.Errorf("empty token: %d", got)
	}
	if got := post(t, f.server.URL, "Bearer "+token); got != http.StatusNoContent {
		t.Errorf("good token: %d", got)
	}
}

func TestNoNodesAcceptsNothing(t *testing.T) {
	f := newFixture(t, nil)
	if got := post(t, f.server.URL, "Bearer "); got != http.StatusUnauthorized {
		t.Errorf("an empty token must never match: %d", got)
	}
}

func TestAllowlist(t *testing.T) {
	allowed := newFixture(t, []testNode{{Name: "worker1", Token: token, Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}})
	if got := post(t, allowed.server.URL, "Bearer "+token); got != http.StatusNoContent {
		t.Errorf("listed address: %d", got)
	}
	denied := newFixture(t, []testNode{{Name: "worker1", Token: token, Allow: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}}})
	if got := post(t, denied.server.URL, "Bearer "+token); got != http.StatusForbidden {
		t.Errorf("unlisted address with a valid token: %d", got)
	}
}

func TestFailedAuthIsRateLimited(t *testing.T) {
	f := newFixture(t, allNodes())
	for range authLimit {
		if got := post(t, f.server.URL, "Bearer bad"); got != http.StatusUnauthorized {
			t.Fatalf("got %d", got)
		}
	}
	if got := post(t, f.server.URL, "Bearer "+token); got != http.StatusTooManyRequests {
		t.Errorf("after too many failures: %d", got)
	}
	f.store.Now = func() time.Time { return time.Now().Add(2 * authWindow) }
	if got := post(t, f.server.URL, "Bearer "+token); got != http.StatusNoContent {
		t.Errorf("after the window: %d", got)
	}
}

func TestNewClientEnforcesTransportRule(t *testing.T) {
	if _, err := control.NewClient("http://8.8.8.8:8200", token); err == nil {
		t.Error("public plain http must be refused")
	}
	if _, err := control.NewClient("https://control.example.com", ""); err == nil {
		t.Error("an empty token must be refused")
	}
}

func TestClientRefusesUntrustedCertificate(t *testing.T) {
	f := newFixture(t, allNodes())
	tlsServer := httptest.NewTLSServer(f.server.Config.Handler)
	defer tlsServer.Close()

	client, err := control.NewClient(tlsServer.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Claim(context.Background()); err == nil {
		t.Error("a self-signed certificate must stop the node")
	}
}

func edgeFixture(t *testing.T) (*fixture, *control.Client, string) {
	t.Helper()
	worker, edgeToken := token, strings.Repeat("e", 32)
	f := newFixture(t, []testNode{{Name: "worker1", Token: worker}, {Name: "edge1", Token: edgeToken, Edge: true}, {Name: "edge2", Token: strings.Repeat("f", 32), Edge: true}})
	f.cfg.EdgeTokenKey = strings.Repeat("k", 32)
	f.cfg.LibraryDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(f.cfg.LibraryDir, "a.mp4"), []byte("video bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	client, err := control.NewClient(f.server.URL, edgeToken)
	if err != nil {
		t.Fatal(err)
	}
	return f, client, strings.Repeat("a", 64)
}

func TestRolesAreSeparate(t *testing.T) {
	f, edgeClient, _ := edgeFixture(t)
	ctx := context.Background()
	f.enqueue(t, "a")
	if _, err := edgeClient.Claim(ctx); err == nil {
		t.Error("an edge node must never take encode jobs")
	}
	if _, err := f.client.EdgePoll(ctx, control.EdgePollRequest{}); err == nil {
		t.Error("a worker must never read edge orders")
	}
}

func TestEdgeOrdersAndAcks(t *testing.T) {
	f, c, hash := edgeFixture(t)
	ctx := context.Background()
	if err := f.store.AddPlacement(&queue.Placement{Node: "edge1", SHA256: hash, Output: "a.mp4", Size: 11}); err != nil {
		t.Fatal(err)
	}
	other := strings.Repeat("b", 64)
	if err := f.store.AddPlacement(&queue.Placement{Node: "edge2", SHA256: other, Output: "b.mp4", Size: 1}); err != nil {
		t.Fatal(err)
	}
	resp, err := c.EdgePoll(ctx, control.EdgePollRequest{DiskFree: 5, DiskTotal: 9})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Orders) != 1 || resp.Orders[0].Verb != control.VerbPull || resp.Orders[0].SHA256 != hash || resp.Orders[0].Token == "" {
		t.Fatalf("orders = %+v", resp.Orders)
	}
	dl, err := c.EdgeDownload(ctx, hash, resp.Orders[0].Token, 6)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(dl.Body)
	dl.Body.Close()
	if dl.StatusCode != http.StatusPartialContent || string(body) != "bytes" {
		t.Errorf("resume = %d %q", dl.StatusCode, body)
	}

	for name, try := range map[string]func() (*http.Response, error){
		"no token":      func() (*http.Response, error) { return c.EdgeDownload(ctx, hash, "", 0) },
		"junk token":    func() (*http.Response, error) { return c.EdgeDownload(ctx, hash, "1.2", 0) },
		"other file":    func() (*http.Response, error) { return c.EdgeDownload(ctx, other, resp.Orders[0].Token, 0) },
		"bad hash path": func() (*http.Response, error) { return c.EdgeDownload(ctx, "..%2f..%2fetc", resp.Orders[0].Token, 0) },
	} {
		got, err := try()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got.Body.Close()
		if got.StatusCode == http.StatusOK || got.StatusCode == http.StatusPartialContent {
			t.Errorf("%s = %d", name, got.StatusCode)
		}
	}

	if err := c.EdgeAck(ctx, control.EdgeAck{Verb: control.VerbPull, SHA256: other}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ActivePlacement("b.mp4", "edge2"); err == nil {
		t.Error("a node must not activate another node's file")
	}
	if err := c.EdgeAck(ctx, control.EdgeAck{Verb: control.VerbPull, SHA256: hash}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ActivePlacement("a.mp4", "edge1"); err != nil {
		t.Errorf("not active: %v", err)
	}
	dl, err = c.EdgeDownload(ctx, hash, resp.Orders[0].Token, 0)
	if err != nil {
		t.Fatal(err)
	}
	dl.Body.Close()
	if dl.StatusCode != http.StatusForbidden {
		t.Errorf("a token must stop working once the pull is over: %d", dl.StatusCode)
	}
}

func TestExpiredDownloadTokenRefused(t *testing.T) {
	f, c, hash := edgeFixture(t)
	ctx := context.Background()
	if err := f.store.AddPlacement(&queue.Placement{Node: "edge1", SHA256: hash, Output: "a.mp4", Size: 11}); err != nil {
		t.Fatal(err)
	}
	resp, err := c.EdgePoll(ctx, control.EdgePollRequest{})
	if err != nil {
		t.Fatal(err)
	}
	f.store.Now = func() time.Time { return time.Now().Add(2 * downloadTokenLife) }
	dl, err := c.EdgeDownload(ctx, hash, resp.Orders[0].Token, 0)
	if err != nil {
		t.Fatal(err)
	}
	dl.Body.Close()
	if dl.StatusCode != http.StatusForbidden {
		t.Errorf("code = %d", dl.StatusCode)
	}
}

func TestMissingFileFailsPlacementAndOrphanIsCleaned(t *testing.T) {
	f, c, hash := edgeFixture(t)
	ctx := context.Background()
	if err := f.store.AddPlacement(&queue.Placement{Node: "edge1", SHA256: hash, Output: "a.mp4", Size: 11}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ActivatePlacement("edge1", hash); err != nil {
		t.Fatal(err)
	}
	orphan := strings.Repeat("c", 64)
	resp, err := c.EdgePoll(ctx, control.EdgePollRequest{Files: []string{orphan, "../not-a-hash"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ActivePlacement("a.mp4", "edge1"); err == nil {
		t.Error("viewers must not be sent to a file the node lacks")
	}
	if len(resp.Orders) != 1 || resp.Orders[0].Verb != control.VerbDelete || resp.Orders[0].SHA256 != orphan {
		t.Errorf("orders = %+v", resp.Orders)
	}
}

func TestEdgeAckRejectsBadInput(t *testing.T) {
	f, c, hash := edgeFixture(t)
	_ = f
	for _, ack := range []control.EdgeAck{{Verb: "exec", SHA256: hash}, {Verb: control.VerbPull, SHA256: "../x"}} {
		if err := c.EdgeAck(context.Background(), ack); err == nil {
			t.Errorf("%+v must be refused", ack)
		}
	}
}

func TestTrustedProxyNamesTheClient(t *testing.T) {
	proxy := netip.MustParsePrefix("10.0.0.1/32")
	cfg := &config.Config{TrustedProxies: []netip.Prefix{proxy}}
	tests := []struct {
		name, remote string
		forwarded    []string
		want         string
	}{
		{"direct client ignores the header", "1.2.3.4:5", []string{"9.9.9.9"}, "1.2.3.4"},
		{"proxy names the client", "10.0.0.1:5", []string{"1.2.3.4"}, "1.2.3.4"},
		{"a forged entry is ignored", "10.0.0.1:5", []string{"8.8.8.8, 1.2.3.4"}, "1.2.3.4"},
		{"a forged header is ignored", "10.0.0.1:5", []string{"8.8.8.8", "1.2.3.4"}, "1.2.3.4"},
		{"proxy without the header", "10.0.0.1:5", nil, "10.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			r.RemoteAddr = tt.remote
			for _, v := range tt.forwarded {
				r.Header.Add("X-Forwarded-For", v)
			}
			got, ok := cfg.ClientAddr(r)
			if !ok || got.String() != tt.want {
				t.Errorf("got %v %v, want %s", got, ok, tt.want)
			}
		})
	}
}

func TestWhoAmI(t *testing.T) {
	edgeToken := strings.Repeat("e", 32)
	f := newFixture(t, []testNode{{Name: "worker1", Token: token}, {Name: "edge1", Token: edgeToken, Edge: true}})
	me, err := f.client.WhoAmI(context.Background())
	if err != nil || me.Name != "worker1" || me.Kind != queue.NodeWorker {
		t.Errorf("worker: %+v, %v", me, err)
	}
	f.enqueue(t, "a")
	edge, err := control.NewClient(f.server.URL, edgeToken)
	if err != nil {
		t.Fatal(err)
	}
	if me, err = edge.WhoAmI(context.Background()); err != nil || me.Name != "edge1" || me.Kind != queue.NodeEdge {
		t.Errorf("edge: %+v, %v", me, err)
	}
	wrong, err := control.NewClient(f.server.URL, strings.Repeat("x", 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.WhoAmI(context.Background()); err == nil {
		t.Error("a wrong token must be refused")
	}
	if claim, err := f.client.Claim(context.Background()); err != nil || claim == nil {
		t.Errorf("whoami must leave the job queued: %v, %v", claim, err)
	}
}

func TestRemovedNodeIsRefused(t *testing.T) {
	f := newFixture(t, allNodes())
	if got := post(t, f.server.URL, "Bearer "+token); got != http.StatusNoContent {
		t.Fatalf("before removal: %d", got)
	}
	if err := f.store.RemoveNode("worker1"); err != nil {
		t.Fatal(err)
	}
	if got := post(t, f.server.URL, "Bearer "+token); got != http.StatusUnauthorized {
		t.Errorf("after removal: %d", got)
	}
}

func TestReleaseGivesTheAttemptBack(t *testing.T) {
	f := newFixture(t, allNodes())
	ctx := context.Background()
	f.enqueue(t, "a")
	claim, err := f.client.Claim(ctx)
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	if err := f.client.Finish(ctx, "a", control.FinishRequest{Lease: claim.Lease, Released: true}); err != nil {
		t.Fatal(err)
	}
	j, err := f.store.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if j.State != queue.StateQueued || j.Attempts != 0 || j.LeaseID != "" {
		t.Errorf("job = %+v", j)
	}
	if err := f.client.Finish(ctx, "a", control.FinishRequest{Lease: claim.Lease, Released: true}); !errors.Is(err, control.ErrLeaseLost) {
		t.Errorf("a second release = %v", err)
	}
}

func TestFailedPullDoesNotUndoAWithdrawal(t *testing.T) {
	f, edgeClient, hash := edgeFixture(t)
	ctx := context.Background()
	if err := f.store.AddPlacement(&queue.Placement{Node: "edge1", SHA256: hash, Output: "a.mp4", Size: 11}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetPlacementState("edge1", hash, queue.PlacementDeleting, ""); err != nil {
		t.Fatal(err)
	}
	if err := edgeClient.EdgeAck(ctx, control.EdgeAck{Verb: control.VerbPull, SHA256: hash, Error: "disk full"}); err != nil {
		t.Fatal(err)
	}
	ps, err := f.store.NodePlacements("edge1")
	if err != nil || len(ps) != 1 || ps[0].State != queue.PlacementDeleting {
		t.Errorf("placements = %+v, %v", ps, err)
	}
	if len(f.edgeFailed) != 0 {
		t.Errorf("a withdrawn copy is no failure: %v", f.edgeFailed)
	}
}

func TestFailedPullIsReported(t *testing.T) {
	f, edgeClient, hash := edgeFixture(t)
	if err := f.store.AddPlacement(&queue.Placement{Node: "edge1", SHA256: hash, Output: "a.mp4", Size: 11}); err != nil {
		t.Fatal(err)
	}
	if err := edgeClient.EdgeAck(context.Background(), control.EdgeAck{Verb: control.VerbPull, SHA256: hash, Error: "disk full"}); err != nil {
		t.Fatal(err)
	}
	if len(f.edgeFailed) != 1 || f.edgeFailed[0] != "edge1:disk full" {
		t.Errorf("reported = %v", f.edgeFailed)
	}
}

func TestControlURLWithPath(t *testing.T) {
	f := newFixture(t, allNodes())
	f.cfg.ControlURL = "https://control.example.com/jf"
	srv := httptest.NewServer(New(f.cfg, f.store, Hooks{}, log.New(io.Discard, "", 0)).Handler())
	t.Cleanup(srv.Close)
	// Behind a proxy that keeps the prefix, and behind one that strips it.
	for _, base := range []string{srv.URL + "/jf", srv.URL} {
		client, err := control.NewClient(base, token)
		if err != nil {
			t.Fatal(err)
		}
		if me, err := client.WhoAmI(context.Background()); err != nil || me.Name != "worker1" {
			t.Errorf("%s: %+v, %v", base, me, err)
		}
	}
	client, err := control.NewClient(srv.URL+"/jfx", token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.WhoAmI(context.Background()); err == nil {
		t.Error("a neighbouring path is not under the prefix")
	}
}
