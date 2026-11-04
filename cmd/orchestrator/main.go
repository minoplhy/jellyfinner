// Command orchestrator runs the central side of the encoding system, and is
// also the operator's CLI for the queue.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"jellyfinner/internal/api"
	"jellyfinner/internal/config"
	"jellyfinner/internal/jellyfin"
	"jellyfinner/internal/notify"
	"jellyfinner/internal/orchestrator"
	"jellyfinner/internal/prepare"
	"jellyfinner/internal/queue"
	"jellyfinner/internal/web"
)

const usage = `usage: orchestrator [--env FILE] COMMAND

  serve
  router                       proxy in front of Jellyfin, a separate process
  health serve|router          exits non-zero if that process is not listening
  doctor                       checks settings, folders, database and Jellyfin
  prepare --owner UID:GID PATH[:MODE]...  creates data folders, run once as root
  node add NAME [--allow CIDR,...]
  node add NAME --edge --url URL --quota-gb N [--allow CIDR,...]
  node list
  node remove NAME
  node rotate NAME             issues a new token, and the old one stops working
  switch-mode auto|manual      run with serve stopped, after changing ENCODE_MODE
  backfill start|pause|status  start needs auto mode
  edge list
  edge pin SOURCE --node NODE
  edge unpin SOURCE --node NODE [--force]
  queue list
  queue add SOURCE
  queue reencode SOURCE --reason TEXT [--force-encode] [--software]
  queue cancel JOB
  queue bump JOB
  queue retry JOB

SOURCE is a path relative to ORIGINALS_DIR.
`

func main() {
	fs := flag.NewFlagSet("orchestrator", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	envFile := fs.String("env", ".env", "settings file")
	_ = fs.Parse(os.Args[1:])
	args := fs.Args()
	if len(args) == 0 {
		fs.Usage()
		os.Exit(2)
	}

	var err error
	switch args[0] {
	case "serve":
		err = serve(*envFile)
	case "router":
		err = router(*envFile)
	case "health":
		err = health(*envFile, args[1:])
	case "doctor":
		err = runDoctor(*envFile)
	case "prepare":
		err = prepare.Command(args[1:])
	case "node":
		err = nodeCommand(*envFile, args[1:])
	case "switch-mode":
		err = switchMode(*envFile, args[1:])
	case "backfill":
		err = backfillCommand(*envFile, args[1:])
	case "edge":
		err = edgeCommand(*envFile, args[1:])
	case "queue":
		err = queueCommand(*envFile, args[1:])
	default:
		fs.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "orchestrator:", err)
		os.Exit(1)
	}
}

// health is for container health checks. The image has no curl.
func health(envFile string, args []string) error {
	cfg, err := config.Load(envFile)
	if err != nil {
		return err
	}
	addr := cfg.ControlAddr
	if len(args) == 1 && args[0] == "router" {
		addr = cfg.RouterAddr
	}
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

func open(envFile string) (*config.Config, *queue.Store, error) {
	cfg, err := config.Load(envFile)
	if err != nil {
		return nil, nil, err
	}
	store, err := queue.Open(cfg.DBPath)
	if err != nil {
		return nil, nil, err
	}
	store.RetryDelay = cfg.RetryDelay
	store.BackfillBudget = cfg.BackfillDailyBytes
	if err := store.CheckMode(string(cfg.Mode)); err != nil {
		store.Close()
		return nil, nil, err
	}
	return cfg, store, nil
}

// waitForFolders covers the moment the prepare service is still running.
func waitForFolders(envFile string) {
	if cfg, err := config.Load(envFile); err == nil {
		prepare.Wait(prepare.WaitFor, filepath.Dir(cfg.DBPath), cfg.IncomingDir, cfg.LibraryDir)
	}
}

func serve(envFile string) error {
	waitForFolders(envFile)
	cfg, store, err := open(envFile)
	if err != nil {
		return err
	}
	defer store.Close()
	release, err := lockServe(cfg.DBPath)
	if err != nil {
		return err
	}
	defer release()
	if cfg.EdgeTokenKey, err = store.EnsureSecret("edge_token_key"); err != nil {
		return err
	}
	if err := cfg.CheckFilesystems(); err != nil {
		return err
	}
	if cfg.TLSCert == "" {
		// The same rule workers enforce, applied to the listening side.
		if err := config.CheckListenAddr(cfg.ControlAddr); err != nil {
			return fmt.Errorf("CONTROL_ADDR without TLS_CERT: %w", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := log.New(os.Stderr, "", log.LstdFlags)
	var library orchestrator.MediaServer
	if cfg.JellyfinURL != "" && cfg.JellyfinToken != "" {
		library = jellyfin.New(cfg.JellyfinURL, cfg.JellyfinToken)
	}
	svc := orchestrator.New(cfg, store, notify.New(cfg.DiscordURL, cfg.NtfyURL), library, logger)
	if cfg.Mode == config.ModeManual {
		if err := svc.Stub.Ensure(); err != nil {
			return fmt.Errorf("stub: %w", err)
		}
	}

	if n, err := store.CancelStaleProfile(svc.Profile); err != nil {
		return err
	} else if n > 0 {
		logger.Printf("output profile changed: %d queued jobs cancelled, backfill rebuilds them", n)
	}

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for range hup {
			svc.TriggerScan()
		}
	}()

	control := api.New(cfg, store, api.Hooks{
		WorkerJob: svc.WorkerJob,
		Completed: svc.TriggerPublish,
		Failed:    func(j *queue.Job) { svc.JobFailed(ctx, j) },
		EdgeFailed: func(node, sha256, reason string) {
			svc.EdgeCopyFailed(ctx, node, sha256, reason)
		},
	}, logger)
	server := &http.Server{
		Addr:              cfg.ControlAddr,
		Handler:           control.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      time.Minute,
	}

	serveErr := make(chan error, 4)
	var webhook *http.Server
	if cfg.Mode == config.ModeManual {
		if err := config.CheckListenAddr(cfg.WebhookAddr); err != nil {
			return fmt.Errorf("WEBHOOK_ADDR: %w", err)
		}
		webhook = &http.Server{Addr: cfg.WebhookAddr, Handler: svc.WebhookHandler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute}
		go func() { serveErr <- webhook.ListenAndServe() }()
	}

	var extra []*http.Server
	if cfg.WebAddr != "" {
		admin := web.New(svc, jellyfin.New(cfg.JellyfinURL, cfg.JellyfinToken), control.LastSeen, logger)
		if cfg.WebTLSCert == "" {
			if err := config.CheckListenAddr(cfg.WebAddr); err != nil {
				return fmt.Errorf("WEB_ADDR without WEB_TLS_CERT: %w", err)
			}
		}
		srv := &http.Server{Addr: cfg.WebAddr, Handler: admin.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute, WriteTimeout: time.Minute}
		extra = append(extra, srv)
		go func() {
			if cfg.WebTLSCert != "" {
				serveErr <- srv.ListenAndServeTLS(cfg.WebTLSCert, cfg.WebTLSKey)
				return
			}
			serveErr <- srv.ListenAndServe()
		}()
	}
	if cfg.MetricsAddr != "" {
		if err := config.CheckListenAddr(cfg.MetricsAddr); err != nil {
			return fmt.Errorf("METRICS_ADDR: %w", err)
		}
		srv := &http.Server{Addr: cfg.MetricsAddr, Handler: svc.MetricsHandler(), ReadHeaderTimeout: 10 * time.Second}
		extra = append(extra, srv)
		go func() { serveErr <- srv.ListenAndServe() }()
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		svc.Run(ctx)
	}()

	go func() {
		if cfg.TLSCert != "" {
			serveErr <- server.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
			return
		}
		serveErr <- server.ListenAndServe()
	}()
	logger.Printf("orchestrator running, mode %s, control API on %s", cfg.Mode, cfg.ControlAddr)
	if nodes, err := store.LoadNodes(); err == nil && len(nodes) == 0 {
		logger.Print("no nodes yet: add a worker with orchestrator node add NAME")
	}

	select {
	case err = <-serveErr:
		stop()
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = server.Shutdown(shutdown)
		if webhook != nil {
			err = errors.Join(err, webhook.Shutdown(shutdown))
		}
		for _, srv := range extra {
			err = errors.Join(err, srv.Shutdown(shutdown))
		}
	}
	wg.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// lockServe holds an advisory lock while serve runs. The kernel drops it if the
// process dies, so a crash cannot leave it stuck.
func lockServe(dbPath string) (func(), error) {
	f, err := os.OpenFile(dbPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("the orchestrator is already running")
	}
	return func() { f.Close() }, nil
}

func switchMode(envFile string, args []string) error {
	if len(args) != 1 {
		return errors.New("switch-mode needs auto or manual")
	}
	to, err := config.ParseMode(args[0])
	if err != nil {
		return err
	}
	cfg, err := config.Load(envFile)
	if err != nil {
		return err
	}
	if cfg.Mode != to {
		return fmt.Errorf("set ENCODE_MODE=%s first, so the next start agrees with this switch", to)
	}
	release, err := lockServe(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("stop serve before switching: %w", err)
	}
	defer release()
	store, err := queue.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()
	current, err := store.Meta("active_mode")
	if err != nil {
		return err
	}
	if current == string(to) {
		return fmt.Errorf("already in %s mode", to)
	}
	n, err := store.SwitchMode(string(to))
	if err != nil {
		return err
	}
	if to == config.ModeManual {
		fmt.Printf("switched to manual: %d queued jobs cancelled, stubs appear on the next scan\n", n)
	} else {
		fmt.Printf("switched to auto: %d unencoded titles moved to the backlog, nothing was queued\n", n)
	}
	return nil
}

func backfillCommand(envFile string, args []string) error {
	if len(args) != 1 {
		return errors.New("backfill needs start, pause or status")
	}
	cfg, store, err := open(envFile)
	if err != nil {
		return err
	}
	defer store.Close()
	svc := orchestrator.New(cfg, store, notify.New("", ""), nil, log.Default())
	switch args[0] {
	case "start":
		if err := svc.SetBackfill(true); err != nil {
			return err
		}
	case "pause":
		if err := svc.SetBackfill(false); err != nil {
			return err
		}
	case "status":
	default:
		return fmt.Errorf("unknown backfill command %q", args[0])
	}
	st, err := svc.BackfillStatus()
	if err != nil {
		return err
	}
	fmt.Printf("active: %v\nremaining: %d titles, %.1f GB\nin progress: %d\nfailed: %d\nused today: %.1f of %.1f GB\n",
		st.Active, st.Remaining, float64(st.RemainingBytes)/1e9, st.InProgress, st.Failed,
		float64(st.UsedToday)/1e9, float64(cfg.BackfillDailyBytes)/1e9)
	return nil
}

func edgeCommand(envFile string, args []string) error {
	if len(args) == 0 {
		return errors.New("edge needs list, pin or unpin")
	}
	cfg, store, err := open(envFile)
	if err != nil {
		return err
	}
	defer store.Close()
	svc := orchestrator.New(cfg, store, notify.New("", ""), nil, log.Default())
	if args[0] == "list" {
		rows, err := svc.NodeRows()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NODE\tSTATE\tSIZE MB\tOUTPUT\tERROR")
		for _, row := range rows {
			for _, p := range row.Placements {
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", p.Node, p.State, p.Size/1e6, p.Output, p.Error)
			}
		}
		return w.Flush()
	}
	if len(args) < 2 {
		return fmt.Errorf("edge %s needs a SOURCE", args[0])
	}
	fs := flag.NewFlagSet("edge", flag.ExitOnError)
	node := fs.String("node", "", "edge node")
	force := fs.Bool("force", false, "remove even if played recently")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	switch args[0] {
	case "pin":
		return svc.Pin(args[1], *node)
	case "unpin":
		return svc.Unpin(args[1], *node, *force)
	}
	return fmt.Errorf("unknown edge command %q", args[0])
}

func queueCommand(envFile string, args []string) error {
	if len(args) == 0 {
		return errors.New("queue needs a command, see --help")
	}
	cfg, store, err := open(envFile)
	if err != nil {
		return err
	}
	defer store.Close()
	svc := orchestrator.New(cfg, store, notify.New("", ""), nil, log.Default())

	command, rest := args[0], args[1:]
	switch command {
	case "list":
		return listJobs(store)
	case "add":
		if len(rest) != 1 {
			return errors.New("queue add needs one SOURCE")
		}
		id, err := svc.Add(rest[0])
		if err != nil {
			return err
		}
		fmt.Println(id)
		return nil
	case "reencode":
		return reencode(svc, rest)
	case "cancel", "bump", "retry":
		if len(rest) != 1 {
			return fmt.Errorf("queue %s needs one JOB", command)
		}
		switch command {
		case "cancel":
			return store.Cancel(rest[0])
		case "bump":
			return store.Bump(rest[0])
		}
		return store.Retry(rest[0])
	}
	return fmt.Errorf("unknown queue command %q", command)
}

func reencode(svc *orchestrator.Service, args []string) error {
	fs := flag.NewFlagSet("reencode", flag.ExitOnError)
	reason := fs.String("reason", "", "what was wrong")
	force := fs.Bool("force-encode", false, "never stream copy the video")
	software := fs.Bool("software", false, "encode with x264 instead of the GPU")
	// Go's flag package stops at the first positional, so take the path first.
	if len(args) == 0 {
		return errors.New("queue reencode needs a SOURCE")
	}
	source := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *reason == "" {
		return errors.New("--reason is required, so repeat problems are visible later")
	}
	id, err := svc.Reencode(source, orchestrator.ReencodeOptions{Reason: *reason, ForceEncode: *force, Software: *software})
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

func listJobs(store *queue.Store) error {
	jobs, err := store.List(100)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "JOB\tSTATE\tPRI\tTRY\tSTAGE\tPATH\tERROR")
	for _, j := range jobs {
		stage := j.Stage
		if j.State == queue.StateRunning {
			stage = fmt.Sprintf("%s %.0f%%", j.Stage, j.Fraction*100)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\t%s\n", j.ID, j.State, j.Priority, j.Attempts, stage, j.Path, j.Error)
	}
	return w.Flush()
}
