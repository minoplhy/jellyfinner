// Package edgeagent is the small program on an edge node. It connects out to
// the orchestrator, and does two things only: pull a file by hash and delete a
// file by hash. An order cannot name a path, a URL or a command.
package edgeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"jellyfinner/internal/control"
	"jellyfinner/internal/edge"
)

// Client is the part of the control API the agent uses.
type Client interface {
	EdgePoll(ctx context.Context, req control.EdgePollRequest) (*control.EdgePollResponse, error)
	EdgeAck(ctx context.Context, req control.EdgeAck) error
	EdgeDownload(ctx context.Context, hash, token string, offset int64) (*http.Response, error)
}

// Agent holds the node's settings. Dir must be readable by whatever serves viewers, and
// only the agent may write to it.
type Agent struct {
	Client  Client
	Dir     string
	Poll    time.Duration
	Limit   int64 // bytes per second, 0 for no cap
	pulling chan struct{}
}

const tmpDir = ".tmp"

// Run polls until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Join(a.Dir, tmpDir), 0o750); err != nil {
		return err
	}
	a.pulling = make(chan struct{}, 1)
	for {
		if err := a.Step(ctx); err == nil {
			control.MarkAlive()
		} else if ctx.Err() == nil {
			log.Printf("poll: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(a.Poll):
		}
	}
}

// Step reports to the orchestrator and acts on any orders. A pull runs in the
// background, so a long transfer does not stop heartbeats or deletes.
func (a *Agent) Step(ctx context.Context) error {
	if a.pulling == nil {
		a.pulling = make(chan struct{}, 1)
	}
	req, err := a.report()
	if err != nil {
		return err
	}
	resp, err := a.Client.EdgePoll(ctx, req)
	if err != nil {
		return err
	}
	for _, order := range resp.Orders {
		a.handle(ctx, order)
	}
	return nil
}

func (a *Agent) handle(ctx context.Context, order control.EdgeOrder) {
	if !edge.ValidHash(order.SHA256) {
		log.Printf("refused order with invalid hash %q", order.SHA256)
		return
	}
	switch order.Verb {
	case control.VerbDelete:
		err := a.remove(order.SHA256)
		a.ack(ctx, order, err)
	case control.VerbPull:
		if order.Size <= 0 {
			log.Printf("refused pull with size %d", order.Size)
			return
		}
		select {
		case a.pulling <- struct{}{}:
		default:
			return // one at a time, and the order comes back on a later poll
		}
		go func() {
			defer func() { <-a.pulling }()
			err := a.pull(ctx, order)
			if ctx.Err() != nil {
				return
			}
			a.ack(ctx, order, err)
		}()
	default:
		log.Printf("refused unknown order %q", order.Verb)
	}
}

func (a *Agent) ack(ctx context.Context, order control.EdgeOrder, err error) {
	ack := control.EdgeAck{Verb: order.Verb, SHA256: order.SHA256}
	if err != nil {
		ack.Error = err.Error()
		log.Printf("%s %s: %v", order.Verb, order.SHA256, err)
	}
	if err := a.Client.EdgeAck(ctx, ack); err != nil {
		log.Printf("ack %s: %v", order.SHA256, err)
	}
}

func (a *Agent) finalPath(hash string) string {
	return filepath.Join(a.Dir, hash+".mp4")
}

func (a *Agent) partPath(hash string) string {
	return filepath.Join(a.Dir, tmpDir, hash+".part")
}

func (a *Agent) remove(hash string) error {
	var errs []error
	for _, f := range []string{a.finalPath(hash), a.partPath(hash)} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// pull resumes a partial file, hashing as it writes, so a verified file needs
// no second read from the disk. Nothing reaches the served name until the hash
// matches.
func (a *Agent) pull(ctx context.Context, order control.EdgeOrder) error {
	final := a.finalPath(order.SHA256)
	if _, err := os.Stat(final); err == nil {
		return nil
	}
	part := a.partPath(order.SHA256)
	f, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	offset, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if offset > order.Size {
		if err := f.Truncate(0); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		h.Reset()
		offset = 0
	}
	if free, _ := diskSpace(a.Dir); free > 0 && free < order.Size-offset {
		return fmt.Errorf("node has %d bytes free, needs %d", free, order.Size-offset)
	}

	if offset < order.Size {
		resp, err := a.Client.EdgeDownload(ctx, order.SHA256, order.Token, offset)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusPartialContent:
		case resp.StatusCode == http.StatusOK && offset == 0:
		default:
			return fmt.Errorf("download: %s", resp.Status)
		}
		// Refuse to accept more than the order said, whatever the orchestrator sends.
		body := io.LimitReader(resp.Body, order.Size-offset+1)
		if _, err := io.Copy(io.MultiWriter(f, h), a.throttle(ctx, body)); err != nil {
			return err
		}
	}

	if got := hex.EncodeToString(h.Sum(nil)); got != order.SHA256 {
		f.Close()
		os.Remove(part)
		return errors.New("hash mismatch, partial file discarded")
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Chmod(0o640); err != nil {
		return err
	}
	return os.Rename(part, final)
}

func (a *Agent) throttle(ctx context.Context, r io.Reader) io.Reader {
	if a.Limit <= 0 {
		return r
	}
	return &limitedReader{ctx: ctx, r: r, limit: a.Limit, start: time.Now()}
}

// limitedReader paces reads to an average rate, so a pull leaves the HDD and
// the uplink free for viewers.
type limitedReader struct {
	ctx   context.Context
	r     io.Reader
	limit int64
	start time.Time
	read  int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if int64(len(p)) > l.limit/4+1 {
		p = p[:l.limit/4+1]
	}
	n, err := l.r.Read(p)
	l.read += int64(n)
	wait := time.Duration(float64(l.read)/float64(l.limit)*float64(time.Second)) - time.Since(l.start)
	if wait > 0 {
		select {
		case <-l.ctx.Done():
			return n, l.ctx.Err()
		case <-time.After(wait):
		}
	}
	return n, err
}

func (a *Agent) report() (control.EdgePollRequest, error) {
	req := control.EdgePollRequest{}
	req.DiskFree, req.DiskTotal = diskSpace(a.Dir)
	entries, err := os.ReadDir(a.Dir)
	if err != nil {
		return req, err
	}
	for _, e := range entries {
		if hash, ok := strings.CutSuffix(e.Name(), ".mp4"); ok && edge.ValidHash(hash) {
			req.Files = append(req.Files, hash)
		}
	}
	return req, nil
}

func diskSpace(dir string) (free, total int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize)
}
