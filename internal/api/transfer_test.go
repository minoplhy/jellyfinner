package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"jellyfinner/internal/control"
	"jellyfinner/internal/transfer"
)

const sourceSize = 70 << 20

// transferFixture has one claimed job, whose source is sourceSize bytes.
func transferFixture(t *testing.T, nodes []testNode) (*fixture, *control.ClaimResponse, []byte) {
	t.Helper()
	f := newFixture(t, nodes)
	f.cfg.OriginalsDir = t.TempDir()
	f.cfg.IncomingDir = t.TempDir()
	source := make([]byte, sourceSize)
	rand.New(rand.NewSource(1)).Read(source)
	if err := os.WriteFile(filepath.Join(f.cfg.OriginalsDir, "a.mkv"), source, 0o640); err != nil {
		t.Fatal(err)
	}
	f.enqueue(t, "a")
	claim, err := f.client.Claim(context.Background())
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	return f, claim, source
}

func newAPI(f *fixture, lease string) *transfer.API {
	return &transfer.API{Client: f.client, Lease: lease, Streams: 4, RetryWait: time.Millisecond}
}

// inFront puts a handler in front of the real one, and points f.client at it.
func inFront(t *testing.T, f *fixture, wrap func(next http.Handler) http.Handler) {
	t.Helper()
	srv := httptest.NewServer(wrap(f.server.Config.Handler))
	t.Cleanup(srv.Close)
	client, err := control.NewClient(srv.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	f.client = client
}

func TestPullAndPush(t *testing.T) {
	f, claim, source := transferFixture(t, allNodes())
	api := newAPI(f, claim.Lease)
	ctx := context.Background()
	j := &claim.Job

	local := filepath.Join(t.TempDir(), "source.mkv")
	if err := api.Pull(ctx, j, local); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, source) {
		t.Error("pulled file differs from the source")
	}
	if entries, _ := filepath.Glob(local + ".part*"); len(entries) != 0 {
		t.Errorf("leftovers: %v", entries)
	}

	if err := api.Push(ctx, j, local, "out.mp4"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(f.cfg.IncomingDir, j.ID, "out.mp4")
	pushed, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pushed, source) {
		t.Error("pushed file differs")
	}
	if info, _ := os.Stat(target); info.Mode().Perm()&0o004 == 0 {
		t.Errorf("Jellyfin must be able to read it, mode %v", info.Mode())
	}

	empty := filepath.Join(t.TempDir(), "empty.srt")
	if err := os.WriteFile(empty, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := api.Push(ctx, j, empty, "empty.srt"); err != nil {
		t.Fatalf("empty file: %v", err)
	}
}

func TestTransferNeedsLiveLease(t *testing.T) {
	nodes := []testNode{{Name: "one", Token: token}, {Name: "two", Token: "token-for-node-two-0123456789abcdef"}}
	f, claim, _ := transferFixture(t, nodes)
	ctx := context.Background()
	id := claim.Job.ID

	if _, err := f.client.SourceSize(ctx, id, claim.Lease); err != nil {
		t.Fatalf("holder refused: %v", err)
	}
	if _, err := f.client.SourceSize(ctx, id, "wrong"); !errors.Is(err, control.ErrLeaseLost) {
		t.Errorf("wrong lease: %v", err)
	}
	if _, err := f.client.SourceSize(ctx, id, ""); !errors.Is(err, control.ErrLeaseLost) {
		t.Errorf("no lease: %v", err)
	}
	if _, err := f.client.SourceSize(ctx, "other", claim.Lease); !errors.Is(err, control.ErrLeaseLost) {
		t.Errorf("other job: %v", err)
	}
	other, err := control.NewClient(f.server.URL, nodes[1].Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.SourceSize(ctx, id, claim.Lease); !errors.Is(err, control.ErrLeaseLost) {
		t.Errorf("other node with a stolen lease: %v", err)
	}
	if err := other.Upload(ctx, id, claim.Lease, "x", 0, bytes.NewReader([]byte("x")), 1); !errors.Is(err, control.ErrLeaseLost) {
		t.Errorf("other node upload: %v", err)
	}

	if err := f.client.Finish(ctx, id, control.FinishRequest{Lease: claim.Lease, Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.SourceSize(ctx, id, claim.Lease); !errors.Is(err, control.ErrLeaseLost) {
		t.Errorf("after the job ended: %v", err)
	}
}

func TestEdgeTokenCannotTransfer(t *testing.T) {
	nodes := []testNode{{Name: "worker1", Token: token}, {Name: "edge1", Token: "token-for-edge-one-0123456789abcdef", Edge: true}}
	f, claim, _ := transferFixture(t, nodes)
	edgeClient, err := control.NewClient(f.server.URL, nodes[1].Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := edgeClient.SourceSize(context.Background(), claim.Job.ID, claim.Lease); err == nil {
		t.Error("an edge node reached a job's source")
	}
}

func TestUploadRefusesEscapingNames(t *testing.T) {
	f, claim, _ := transferFixture(t, allNodes())
	ctx := context.Background()
	for _, name := range []string{"../escape", "..", ".", `a\b`, "a/b"} {
		err := f.client.Upload(ctx, claim.Job.ID, claim.Lease, name, 0, bytes.NewReader([]byte("x")), 1)
		if err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
	if _, err := os.Stat(filepath.Join(f.cfg.IncomingDir, "escape")); err == nil {
		t.Error("a file escaped the job folder")
	}
}

func TestUploadOffsetRules(t *testing.T) {
	f, claim, _ := transferFixture(t, allNodes())
	ctx := context.Background()
	id := claim.Job.ID
	put := func(offset int64, body string) error {
		return f.client.Upload(ctx, id, claim.Lease, "f", offset, bytes.NewReader([]byte(body)), int64(len(body)))
	}
	if err := put(0, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := put(99, "x"); err == nil {
		t.Error("a gap was accepted")
	}
	if err := put(5, " world"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(f.cfg.IncomingDir, id, "f"))
	if string(got) != "hello world" {
		t.Errorf("got %q", got)
	}
	// A new attempt starts at 0 and must not keep the old content.
	if err := put(0, "new"); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(filepath.Join(f.cfg.IncomingDir, id, "f"))
	if string(got) != "new" {
		t.Errorf("after restart got %q", got)
	}
}

func TestPullResumes(t *testing.T) {
	f, claim, source := transferFixture(t, allNodes())
	var failing atomic.Bool
	var served, requests atomic.Int64
	failing.Store(true)
	inFront(t, f, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Range") != "" {
				requests.Add(1)
				if failing.Load() && served.Add(1) > 1 {
					http.Error(w, "down", http.StatusBadGateway)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	api := newAPI(f, claim.Lease)
	api.Streams = 1
	local := filepath.Join(t.TempDir(), "source.mkv")
	ctx := context.Background()
	if err := api.Pull(ctx, &claim.Job, local); err == nil {
		t.Fatal("pull should fail while the server is down")
	}
	if _, err := os.Stat(local); err == nil {
		t.Fatal("a partial pull took the final name")
	}

	failing.Store(false)
	requests.Store(0)
	if err := api.Pull(ctx, &claim.Job, local); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("resume fetched %d chunks, want the 2 missing", got)
	}
	got, _ := os.ReadFile(local)
	if !bytes.Equal(got, source) {
		t.Error("resumed file differs")
	}

	// A finished pull is reused without fetching anything.
	requests.Store(0)
	if err := api.Pull(ctx, &claim.Job, local); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("a complete source was fetched again, %d requests", got)
	}
}

func TestPushResumes(t *testing.T) {
	f, claim, source := transferFixture(t, allNodes())
	var cut atomic.Bool
	var resumedFrom atomic.Int64
	inFront(t, f, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut {
				if cut.CompareAndSwap(false, true) {
					r.Body = io.NopCloser(&brokenReader{r: r.Body, left: 10 << 20})
				} else if offset, _ := strconv.ParseInt(r.Header.Get(control.OffsetHeader), 10, 64); offset > 0 {
					resumedFrom.Store(offset)
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	local := filepath.Join(t.TempDir(), "out.mp4")
	if err := os.WriteFile(local, source, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := newAPI(f, claim.Lease).Push(context.Background(), &claim.Job, local, "out.mp4"); err != nil {
		t.Fatal(err)
	}
	if resumedFrom.Load() == 0 {
		t.Error("the upload restarted from zero")
	}
	got, _ := os.ReadFile(filepath.Join(f.cfg.IncomingDir, claim.Job.ID, "out.mp4"))
	if !bytes.Equal(got, source) {
		t.Error("resumed upload differs")
	}
}

type brokenReader struct {
	r    io.Reader
	left int64
}

func (b *brokenReader) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errors.New("connection lost")
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

func TestLostLeaseStopsPull(t *testing.T) {
	f, claim, _ := transferFixture(t, allNodes())
	var ranges atomic.Int64
	inFront(t, f, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Range") != "" {
				ranges.Add(1)
				http.Error(w, "lease lost", http.StatusConflict)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	api := newAPI(f, claim.Lease)
	api.Streams = 1
	err := api.Pull(context.Background(), &claim.Job, filepath.Join(t.TempDir(), "s"))
	if !errors.Is(err, control.ErrLeaseLost) {
		t.Fatalf("err = %v", err)
	}
	if ranges.Load() != 1 {
		t.Errorf("kept trying after losing the lease: %d requests", ranges.Load())
	}
}
