// Command worker runs encode jobs on the encoding node. It only connects out
// to the orchestrator and pulls work.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"jellyfinner/internal/control"
	"jellyfinner/internal/encode"
	"jellyfinner/internal/pipeline"
	"jellyfinner/internal/prepare"
	"jellyfinner/internal/transfer"
)

const (
	usage          = "usage: worker serve --url URL --scratch DIR [--transfer https|rclone] [--backend software|vaapi] [--device PATH]\n       worker doctor\n       worker health\n       worker prepare --owner UID:GID PATH[:MODE]...\n\nThe control token is read from NODE_TOKEN. Settings can also come from ORCHESTRATOR_URL, SCRATCH_DIR, TRANSFER, ENCODER, VAAPI_DEVICE, FFMPEG and FFPROBE.\n"
	logTailBytes   = 256 << 10
	finishAttempts = 5
	healthMaxAge   = 3 * time.Minute
	// scratchMaxAge clears what a crashed job left, once it is too old to be
	// the retry that would reuse it.
	scratchMaxAge = 6 * time.Hour
)

type worker struct {
	client    *control.Client
	cfg       pipeline.Config
	poll      time.Duration
	heartbeat time.Duration
	method    string
	rclone    string
	streams   int
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "health" {
		if err := control.CheckAlive(healthMaxAge); err != nil {
			fmt.Fprintln(os.Stderr, "worker:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) >= 2 && (os.Args[1] == "doctor" || os.Args[1] == "prepare") {
		var err error
		if os.Args[1] == "doctor" {
			err = runDoctor(os.Args[2:])
		} else {
			err = prepare.Command(os.Args[2:])
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "worker:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		os.Exit(1)
	}
}

// settings are the flags serve and doctor share. Each falls back to the
// environment, so a compose file needs no command line.
type settings struct {
	url, scratch, backend, device, ffmpeg, ffprobe, method, rclone *string
}

func flags(fs *flag.FlagSet) settings {
	return settings{
		url:     fs.String("url", os.Getenv("ORCHESTRATOR_URL"), "orchestrator control URL"),
		scratch: fs.String("scratch", os.Getenv("SCRATCH_DIR"), "scratch directory on fast local disk"),
		backend: fs.String("backend", envOr("ENCODER", string(encode.BackendSoftware)), "software or vaapi"),
		device:  fs.String("device", os.Getenv("VAAPI_DEVICE"), "VAAPI render node"),
		ffmpeg:  fs.String("ffmpeg", envOr("FFMPEG", "ffmpeg"), "ffmpeg binary, ideally jellyfin-ffmpeg"),
		ffprobe: fs.String("ffprobe", envOr("FFPROBE", "ffprobe"), "ffprobe binary"),
		method:  fs.String("transfer", envOr("TRANSFER", "https"), "https uses the control API, rclone uses REMOTE_ORIGINALS and REMOTE_INCOMING"),
		rclone:  fs.String("rclone", "rclone", "rclone binary"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	set := flags(fs)
	url, scratch, backend, device, ffmpeg, ffprobe, method, rclone := set.url, set.scratch, set.backend, set.device, set.ffmpeg, set.ffprobe, set.method, set.rclone
	streams := fs.Int("streams", 8, "parallel streams per file")
	poll := fs.Duration("poll", 10*time.Second, "how often to ask for work")
	heartbeat := fs.Duration("heartbeat", 15*time.Second, "how often to renew a lease")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *scratch == "" {
		return errors.New("--scratch is required")
	}
	if b := encode.Backend(*backend); b != encode.BackendVAAPI && b != encode.BackendSoftware {
		return fmt.Errorf("unknown backend %q", *backend)
	}
	if *method != "https" && *method != "rclone" {
		return fmt.Errorf("unknown transfer %q", *method)
	}
	prepare.Wait(prepare.WaitFor, *scratch)
	if err := os.MkdirAll(*scratch, 0o750); err != nil {
		return err
	}
	client, err := control.NewClient(*url, os.Getenv("NODE_TOKEN"))
	if err != nil {
		return err
	}

	profile := encode.DefaultProfile()
	if *device != "" {
		profile.Device = *device
	}
	w := &worker{
		client:    client,
		poll:      *poll,
		heartbeat: *heartbeat,
		method:    *method,
		rclone:    *rclone,
		streams:   *streams,
		cfg: pipeline.Config{
			Scratch: *scratch,
			FFmpeg:  *ffmpeg,
			FFprobe: *ffprobe,
			Profile: profile,
			Backend: encode.Backend(*backend),
		},
	}
	return w.loop(ctx)
}

func (w *worker) loop(ctx context.Context) error {
	for {
		claim, err := w.client.Claim(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			log.Printf("claim: %v", err)
		} else {
			control.MarkAlive()
		}
		if claim != nil {
			w.runJob(ctx, claim)
			continue
		}
		sweepScratch(w.cfg.Scratch, scratchMaxAge)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(w.poll):
		}
	}
}

func sweepScratch(scratch string, maxAge time.Duration) {
	entries, err := os.ReadDir(scratch)
	if err != nil {
		log.Printf("sweep scratch: %v", err)
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < maxAge {
			continue
		}
		if err := os.RemoveAll(filepath.Join(scratch, e.Name())); err != nil {
			log.Printf("sweep scratch: %v", err)
		}
	}
}

func (w *worker) runJob(ctx context.Context, claim *control.ClaimResponse) {
	id := claim.Job.ID
	log.Printf("job %s: %s", id, claim.Job.Source)
	pruneScratch(w.cfg.Scratch, id)

	jobCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	tail := &tailWriter{limit: logTailBytes}
	cfg := w.cfg
	if w.method == "rclone" {
		cfg.Transfer = &transfer.Rclone{Binary: w.rclone, Streams: w.streams, Log: tail}
	} else {
		cfg.Transfer = &transfer.API{Client: w.client, Lease: claim.Lease, Streams: w.streams}
	}

	status := &progress{}
	var beat sync.WaitGroup
	stopBeat := make(chan struct{})
	beat.Add(1)
	go func() {
		defer beat.Done()
		w.keepAlive(jobCtx, id, claim.Lease, status, stopBeat, cancel)
	}()

	manifest, err := pipeline.Run(jobCtx, cfg, &claim.Job, tail, status.set)
	close(stopBeat)
	beat.Wait()

	// A shutdown has already cancelled ctx, so report on a fresh one.
	reportCtx, stopReport := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer stopReport()
	if cause := context.Cause(jobCtx); errors.Is(cause, control.ErrLeaseLost) {
		log.Printf("job %s: lease lost, dropping the result", id)
		return
	}
	req := control.FinishRequest{Lease: claim.Lease, Log: tail.String()}
	if err != nil {
		req.Error = err.Error()
		log.Printf("job %s failed: %v", id, err)
	} else {
		data, merr := json.Marshal(manifest)
		if merr != nil {
			req.Error = merr.Error()
		}
		req.Manifest = data
	}
	w.report(reportCtx, id, req)
}

func (w *worker) keepAlive(ctx context.Context, id, lease string, status *progress, stop <-chan struct{}, cancel context.CancelCauseFunc) {
	ticker := time.NewTicker(w.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
		}
		stage, fraction := status.get()
		stopJob, err := w.client.Heartbeat(ctx, id, control.HeartbeatRequest{Lease: lease, Stage: stage, Fraction: fraction})
		switch {
		case errors.Is(err, control.ErrLeaseLost):
			cancel(control.ErrLeaseLost)
			return
		case err != nil:
			// A brief network drop must not kill a long encode. If it lasts,
			// the lease expires and the next heartbeat reports that.
			log.Printf("job %s heartbeat: %v", id, err)
		case stopJob:
			log.Printf("job %s cancelled by the orchestrator", id)
			cancel(context.Canceled)
			return
		default:
			control.MarkAlive()
		}
	}
}

// report retries, because losing a finished encode to a brief network drop
// would waste the whole job.
func (w *worker) report(ctx context.Context, id string, req control.FinishRequest) {
	delay := 5 * time.Second
	for attempt := 1; ; attempt++ {
		err := w.client.Finish(ctx, id, req)
		if err == nil {
			return
		}
		if errors.Is(err, control.ErrLeaseLost) || attempt == finishAttempts {
			log.Printf("job %s: could not report the result: %v", id, err)
			return
		}
		log.Printf("job %s: report failed, retrying: %v", id, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// pruneScratch clears leftovers from other jobs. The current job's directory
// stays, because a retry can reuse its pulled source.
func pruneScratch(scratch, keep string) {
	entries, err := os.ReadDir(scratch)
	if err != nil {
		log.Printf("prune scratch: %v", err)
		return
	}
	for _, e := range entries {
		if e.Name() == keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(scratch, e.Name())); err != nil {
			log.Printf("prune scratch: %v", err)
		}
	}
}

type progress struct {
	mu       sync.Mutex
	stage    string
	fraction float64
}

func (p *progress) set(stage string, fraction float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stage, p.fraction = stage, fraction
}

func (p *progress) get() (string, float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stage, p.fraction
}

// tailWriter keeps the end of the log, where the failure is.
type tailWriter struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.limit:]...)
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
