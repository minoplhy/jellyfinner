package edgeagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyfinner/internal/control"
)

type fakeClient struct {
	mu     sync.Mutex
	orders []control.EdgeOrder
	acks   []control.EdgeAck
	polls  []control.EdgePollRequest
	data   []byte
	ranges []int64
	status int
	block  chan struct{}
}

func (f *fakeClient) EdgePoll(_ context.Context, req control.EdgePollRequest) (*control.EdgePollResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls = append(f.polls, req)
	return &control.EdgePollResponse{Orders: f.orders}, nil
}

func (f *fakeClient) EdgeAck(_ context.Context, a control.EdgeAck) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acks = append(f.acks, a)
	return nil
}

func (f *fakeClient) EdgeDownload(_ context.Context, _, _ string, offset int64) (*http.Response, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ranges = append(f.ranges, offset)
	status := http.StatusOK
	if offset > 0 {
		status = http.StatusPartialContent
	}
	if f.status != 0 {
		status = f.status
	}
	return &http.Response{StatusCode: status, Status: strconv.Itoa(status), Body: io.NopCloser(bytes.NewReader(f.data[offset:]))}, nil
}

func (f *fakeClient) ackCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.acks)
}

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newAgent(t *testing.T, client *fakeClient) *Agent {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, tmpDir), 0o750); err != nil {
		t.Fatal(err)
	}
	return &Agent{Client: client, Dir: dir, pulling: make(chan struct{}, 1)}
}

func step(t *testing.T, a *Agent, client *fakeClient, wantAcks int) {
	t.Helper()
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for client.ackCount() < wantAcks && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := client.ackCount(); got != wantAcks {
		t.Fatalf("acks = %d, want %d", got, wantAcks)
	}
}

func TestPullVerifiesAndPublishes(t *testing.T) {
	data := []byte("encoded video")
	hash := hashOf(data)
	client := &fakeClient{data: data, orders: []control.EdgeOrder{{Verb: control.VerbPull, SHA256: hash, Size: int64(len(data)), Token: "t"}}}
	a := newAgent(t, client)
	step(t, a, client, 1)

	if client.acks[0].Error != "" {
		t.Fatal(client.acks[0].Error)
	}
	got, err := os.ReadFile(filepath.Join(a.Dir, hash+".mp4"))
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("file = %q, %v", got, err)
	}
	if _, err := os.Stat(a.partPath(hash)); !os.IsNotExist(err) {
		t.Error("the partial file must be gone")
	}
}

func TestBadHashLeavesNothing(t *testing.T) {
	data := []byte("tampered")
	hash := strings.Repeat("a", 64)
	client := &fakeClient{data: data, orders: []control.EdgeOrder{{Verb: control.VerbPull, SHA256: hash, Size: int64(len(data))}}}
	a := newAgent(t, client)
	step(t, a, client, 1)

	if !strings.Contains(client.acks[0].Error, "hash mismatch") {
		t.Errorf("ack = %+v", client.acks[0])
	}
	entries, err := os.ReadDir(a.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".mp4") {
			t.Errorf("unverified file on disk: %s", e.Name())
		}
	}
	if _, err := os.Stat(a.partPath(hash)); !os.IsNotExist(err) {
		t.Error("a bad partial must be discarded")
	}
}

func TestOversizedResponseIsRejected(t *testing.T) {
	data := []byte("declared small but actually much longer")
	hash := hashOf(data[:8])
	client := &fakeClient{data: data, orders: []control.EdgeOrder{{Verb: control.VerbPull, SHA256: hash, Size: 8}}}
	a := newAgent(t, client)
	step(t, a, client, 1)
	if client.acks[0].Error == "" {
		t.Error("more bytes than ordered must fail")
	}
	info, err := os.Stat(a.partPath(hash))
	if err == nil && info.Size() > 9 {
		t.Errorf("wrote %d bytes for an 8 byte order", info.Size())
	}
}

func TestPullResumesAtOffset(t *testing.T) {
	data := []byte("0123456789abcdefghij")
	hash := hashOf(data)
	client := &fakeClient{data: data, orders: []control.EdgeOrder{{Verb: control.VerbPull, SHA256: hash, Size: int64(len(data))}}}
	a := newAgent(t, client)
	if err := os.WriteFile(a.partPath(hash), data[:7], 0o640); err != nil {
		t.Fatal(err)
	}
	step(t, a, client, 1)
	if client.acks[0].Error != "" {
		t.Fatal(client.acks[0].Error)
	}
	if len(client.ranges) != 1 || client.ranges[0] != 7 {
		t.Errorf("ranges = %v", client.ranges)
	}
	if got, _ := os.ReadFile(a.finalPath(hash)); !bytes.Equal(got, data) {
		t.Errorf("file = %q", got)
	}
}

func TestMalformedOrdersDoNothing(t *testing.T) {
	victim := "keep.txt"
	orders := []control.EdgeOrder{
		{Verb: control.VerbDelete, SHA256: "../" + victim},
		{Verb: control.VerbDelete, SHA256: "/etc/passwd"},
		{Verb: control.VerbDelete, SHA256: strings.Repeat("a", 63) + "/"},
		{Verb: control.VerbDelete, SHA256: strings.ToUpper(strings.Repeat("a", 64))},
		{Verb: control.VerbDelete, SHA256: ""},
		{Verb: "exec", SHA256: strings.Repeat("a", 64)},
		{Verb: "", SHA256: strings.Repeat("a", 64)},
		{Verb: control.VerbPull, SHA256: "..", Size: 5},
		{Verb: control.VerbPull, SHA256: strings.Repeat("a", 64), Size: 0},
		{Verb: control.VerbPull, SHA256: strings.Repeat("a", 64), Size: -1},
	}
	client := &fakeClient{orders: orders}
	a := newAgent(t, client)
	outside := filepath.Join(filepath.Dir(a.Dir), victim)
	if err := os.WriteFile(outside, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)
	inside := filepath.Join(a.Dir, victim)
	if err := os.WriteFile(inside, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if len(client.ranges) != 0 {
		t.Error("a malformed order caused a download")
	}
	if client.ackCount() != 0 {
		t.Errorf("acks = %+v", client.acks)
	}
	for _, f := range []string{outside, inside} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s was touched: %v", f, err)
		}
	}
}

func TestDeleteOnlyRemovesThatHash(t *testing.T) {
	hash, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	client := &fakeClient{orders: []control.EdgeOrder{{Verb: control.VerbDelete, SHA256: hash}}}
	a := newAgent(t, client)
	for _, f := range []string{a.finalPath(hash), a.partPath(hash), a.finalPath(other)} {
		if err := os.WriteFile(f, []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	step(t, a, client, 1)
	if client.acks[0].Error != "" {
		t.Fatal(client.acks[0].Error)
	}
	if _, err := os.Stat(a.finalPath(hash)); !os.IsNotExist(err) {
		t.Error("file remains")
	}
	if _, err := os.Stat(a.finalPath(other)); err != nil {
		t.Error("another file was deleted")
	}
}

func TestDeleteMissingFileSucceeds(t *testing.T) {
	client := &fakeClient{orders: []control.EdgeOrder{{Verb: control.VerbDelete, SHA256: strings.Repeat("c", 64)}}}
	a := newAgent(t, client)
	step(t, a, client, 1)
	if client.acks[0].Error != "" {
		t.Error("deleting an absent file must succeed, or the order repeats forever")
	}
}

func TestOnePullAtATimeAndHeartbeatsContinue(t *testing.T) {
	one, two := []byte("first file"), []byte("second file")
	client := &fakeClient{data: one, block: make(chan struct{}), orders: []control.EdgeOrder{
		{Verb: control.VerbPull, SHA256: hashOf(one), Size: int64(len(one))},
		{Verb: control.VerbPull, SHA256: hashOf(two), Size: int64(len(two))},
	}}
	a := newAgent(t, client)
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.polls) != 2 {
		t.Errorf("a pull must not block heartbeats: %d polls", len(client.polls))
	}
	close(client.block)
	deadline := time.Now().Add(3 * time.Second)
	for client.ackCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.ranges) != 1 {
		t.Errorf("downloads = %d, want one at a time", len(client.ranges))
	}
}

func TestReportListsOnlyHashNamedFiles(t *testing.T) {
	client := &fakeClient{}
	a := newAgent(t, client)
	hash := strings.Repeat("d", 64)
	for _, name := range []string{hash + ".mp4", "notes.mp4", hash + ".txt"} {
		if err := os.WriteFile(filepath.Join(a.Dir, name), nil, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	req, err := a.report()
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Files) != 1 || req.Files[0] != hash || req.DiskTotal == 0 {
		t.Errorf("report = %+v", req)
	}
}

func TestThrottlePaces(t *testing.T) {
	r := &limitedReader{ctx: context.Background(), r: bytes.NewReader(make([]byte, 2000)), limit: 10000, start: time.Now()}
	start := time.Now()
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("2000 bytes at 10 kB/s took %v", d)
	}
}
