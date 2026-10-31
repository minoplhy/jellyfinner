package transfer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"jellyfinner/internal/control"
	"jellyfinner/internal/job"
)

const (
	chunkSize      = 32 << 20
	maxAttempts    = 6
	firstRetryWait = time.Second
)

// API moves files through the control API over HTTP. Parallel connections
// fill a long path that one TCP stream cannot, and every transfer resumes.
// The lease limits it to the current job.
type API struct {
	Client  *control.Client
	Lease   string
	Streams int

	// RetryWait replaces the first retry delay in tests.
	RetryWait time.Duration
}

func (a *API) wait(ctx context.Context, attempt int) error {
	delay := firstRetryWait
	if a.RetryWait > 0 {
		delay = a.RetryWait
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay << attempt):
		return nil
	}
}

// retry stops at once for a lost lease or a cancelled job.
func (a *API) retry(ctx context.Context, step func() error) error {
	var err error
	for attempt := range maxAttempts {
		if err = step(); err == nil || errors.Is(err, control.ErrLeaseLost) || ctx.Err() != nil {
			return err
		}
		if attempt < maxAttempts-1 {
			if werr := a.wait(ctx, attempt); werr != nil {
				return werr
			}
		}
	}
	return err
}

// Size returns the size of the job's source.
func (a *API) Size(ctx context.Context, j *job.Job) (int64, error) {
	var size int64
	err := a.retry(ctx, func() error {
		var err error
		size, err = a.Client.SourceSize(ctx, j.ID, a.Lease)
		return err
	})
	return size, err
}

// Pull downloads the source in chunks over parallel connections. The file
// takes its final name only when complete, and a map beside it records
// finished chunks, so an interrupted pull continues where it stopped.
func (a *API) Pull(ctx context.Context, j *job.Job, local string) error {
	size, err := a.Size(ctx, j)
	if err != nil {
		return err
	}
	if info, err := os.Stat(local); err == nil && info.Size() == size {
		return nil
	}
	part, mapPath := local+".part", local+".part.map"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		return err
	}
	done, err := loadChunks(mapPath)
	if err != nil {
		return err
	}
	record, err := os.OpenFile(mapPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	defer record.Close()

	total := int((size + chunkSize - 1) / chunkSize)
	work := make(chan int)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for range max(1, a.Streams) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunk := range work {
				start := int64(chunk) * chunkSize
				length := min(chunkSize, size-start)
				err := a.retry(ctx, func() error { return a.pullChunk(ctx, j, f, start, length) })
				if err == nil {
					// Sync first, so the map never claims data the disk lacks.
					if err = f.Sync(); err == nil {
						mu.Lock()
						_, err = fmt.Fprintln(record, chunk)
						mu.Unlock()
					}
				}
				if err != nil {
					cancel(err)
					return
				}
			}
		}()
	}
feed:
	for chunk := range total {
		if done[chunk] {
			continue
		}
		select {
		case work <- chunk:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(part, local); err != nil {
		return err
	}
	return os.Remove(mapPath)
}

func (a *API) pullChunk(ctx context.Context, j *job.Job, f *os.File, start, length int64) error {
	body, err := a.Client.SourceRange(ctx, j.ID, a.Lease, start, length)
	if err != nil {
		return err
	}
	defer body.Close()
	n, err := io.Copy(io.NewOffsetWriter(f, start), io.LimitReader(body, length))
	if err != nil {
		return err
	}
	if n != length {
		return fmt.Errorf("short chunk at %d: got %d of %d bytes", start, n, length)
	}
	return nil
}

func loadChunks(path string) (map[int]bool, error) {
	done := map[int]bool{}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return done, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		// A torn last line only means that chunk is fetched again.
		if chunk, err := strconv.Atoi(strings.TrimSpace(lines.Text())); err == nil {
			done[chunk] = true
		}
	}
	return done, lines.Err()
}

// Push uploads a file, continuing from what arrived if the connection drops.
func (a *API) Push(ctx context.Context, j *job.Job, local, name string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	var offset int64
	return a.retry(ctx, func() error {
		err := a.Client.Upload(ctx, j.ID, a.Lease, name, offset, io.NewSectionReader(f, offset, size-offset), size-offset)
		if err == nil || errors.Is(err, control.ErrLeaseLost) {
			return err
		}
		// A failed check keeps the old offset. The server refuses a mismatch,
		// and offset 0 is always safe.
		if held, oerr := a.Client.UploadOffset(ctx, j.ID, a.Lease, name); oerr == nil && held <= size {
			offset = held
		} else if oerr == nil {
			offset = 0
		}
		return err
	})
}
