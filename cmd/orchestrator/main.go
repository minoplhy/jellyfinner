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
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"jellyfinner/internal/api"
	"jellyfinner/internal/buildinfo"
	"jellyfinner/internal/config"
	"jellyfinner/internal/database"
	"jellyfinner/internal/jellyfin"
	"jellyfinner/internal/model"
	"jellyfinner/internal/nodes"
	"jellyfinner/internal/notify"
	"jellyfinner/internal/orchestrator"
	"jellyfinner/internal/prepare"
	"jellyfinner/internal/web"
)

const usage = `usage: orchestrator [--env FILE] COMMAND

  serve
  health serve                 exits non-zero if serve is not listening
  doctor                       checks settings, folders, database and Jellyfin
  prepare --owner UID:GID PATH[:MODE]...  creates data folders, run once as root
  version                      prints the build
  node add NAME [--allow CIDR,...]
  node add NAME --edge --url URL --quota-gb N [--allow CIDR,...]
  node add NAME --router [--allow CIDR,...]   in front of Jellyfin, see compose.router.yml
  node list
  node remove NAME
  node edit NAME [--name NEW] [--allow CIDR,...] [--url URL] [--quota-gb N]
                               changes a node; its token and keys stay
  node rotate NAME             issues a new token, and the old one stops working
  node pause NAME|--all        a worker finishes its job, then takes no more
  node resume NAME|--all
  cloud status                 rented workers, billed hours and estimated spending
  cloud up [--provider NAME]   start one now, within the limits
  cloud drain NAME             finish its job, then delete it
  cloud down [--wait]          drain all; --wait blocks until deletes are confirmed
  cloud pause|resume           stop or allow new servers
  cloud reconcile              compare each provider's list with ours
  cloud estimate               prices, measured speed and cost to clear the queue
  usage [--month YYYY-MM]      a month's work, traffic and cloud spending, and the backlog
  switch-mode auto|manual      run with serve stopped, after changing ENCODE_MODE
  backfill start|pause|status  start needs auto mode
  edge list
  edge pin SOURCE --node NODE
  edge unpin SOURCE --node NODE [--force]
  edge viewers                 which Jellyfin users the router sends to which edge node
  edge assign USER --node NODE sends a user to an edge node, moving them from any other
  edge unassign USER           sends a user back to the origin
  queue list
  queue add SOURCE
  queue reencode SOURCE --reason TEXT [--force-encode] [--software]
  queue requeue JOB [--pause]  take a running job from its worker and queue it again
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
	case "health":
		err = health(*envFile, args[1:])
	case "doctor":
		err = runDoctor(*envFile)
	case "prepare":
		err = prepare.Command(args[1:])
	case "version":
		fmt.Println(buildinfo.Line("orchestrator"))
	case "cloud":
		err = cloudCommand(*envFile, args[1:])
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
	case "usage":
		err = usageCommand(*envFile, args[1:])
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
	conn, err := net.DialTimeout("tcp", cfg.ControlAddr, 5*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

func open(envFile string) (*config.Config, database.Store, error) {
	cfg, err := config.Load(envFile)
	if err != nil {
		return nil, nil, err
	}
	store, err := openStore(cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := orchestrator.CheckMode(store, string(cfg.Mode)); err != nil {
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
	log.Print(buildinfo.Line("orchestrator"))
	if cfg.EdgeTokenKey, err = nodes.EnsureSecret(store, "edge_token_key"); err != nil {
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
	var library jellyfin.API
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

	control := api.New(cfg, store, svc.Nodes, api.Hooks{
		WorkerJob: svc.WorkerJob,
		Completed: svc.TriggerPublish,
		Failed:    func(j *model.Job) { svc.JobFailed(ctx, j) },
		EdgeFailed: func(node, sha256, reason string) {
			svc.EdgeCopyFailed(ctx, node, sha256, reason)
		},
		Route: svc.EdgeRedirect,
	}, logger)
	if err := orchestrator.CheckCloudProviders(svc); err != nil {
		return err
	}
	if cfg.Cloud.Enabled() {
		providers, err := orchestrator.BuildProviders(cfg.Cloud)
		if err != nil {
			return err
		}
		if svc.Cloud, err = orchestrator.NewCloud(svc, providers, control.LastSeen); err != nil {
			return err
		}
		logger.Printf("cloud workers on, providers in this order: %s", strings.Join(providerNames(providers), ", "))
	}
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
	if cfg.AdminAddr != "" {
		admin := web.New(svc, jellyfin.New(cfg.JellyfinURL, cfg.JellyfinToken), control.LastSeen, logger)
		admin.NodeReports = control.NodeReports
		if cfg.AdminTLSCert == "" {
			if err := config.CheckListenAddr(cfg.AdminAddr); err != nil {
				return fmt.Errorf("ADMIN_ADDR without ADMIN_TLS_CERT: %w", err)
			}
		}
		srv := &http.Server{Addr: cfg.AdminAddr, Handler: admin.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute, WriteTimeout: time.Minute}
		extra = append(extra, srv)
		go func() {
			if cfg.AdminTLSCert != "" {
				serveErr <- srv.ListenAndServeTLS(cfg.AdminTLSCert, cfg.AdminTLSKey)
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
	if cfg.LegacyEdgeViewers {
		logger.Print(orchestrator.LegacyViewersNote)
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
	store, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	current, err := store.Meta(model.MetaActiveMode)
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
		return errors.New("edge needs list, pin, unpin, viewers, assign or unassign")
	}
	cfg, store, err := open(envFile)
	if err != nil {
		return err
	}
	defer store.Close()
	svc := orchestrator.New(cfg, store, notify.New("", ""), nil, log.Default())
	switch args[0] {
	case "viewers":
		return listViewers(svc)
	case "assign", "unassign":
		return assignViewer(svc, args[0] == "assign", args[1:])
	}
	if args[0] == "list" {
		rows, err := svc.NodeRows()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NODE\tSTATE\tSIZE MB\tOUTPUT\tERROR")
		for _, row := range rows {
			for _, p := range row.Placements {
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", row.Name, p.State, p.Size/1e6, p.Output, p.Error)
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
	if args[0] != "pin" && args[0] != "unpin" {
		return fmt.Errorf("unknown edge command %q", args[0])
	}
	n, err := svc.FindNode(*node)
	if err != nil {
		return err
	}
	if args[0] == "pin" {
		return svc.Pin(args[1], n.ID)
	}
	return svc.Unpin(args[1], n.ID, *force)
}

func listViewers(svc *orchestrator.Service) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "USER\tEDGE")
	viewers := svc.Nodes.Viewers()
	for _, v := range viewers {
		fmt.Fprintf(w, "%s\t%s\n", v.Username, svc.NodeName(v.Node))
	}
	if len(viewers) == 0 {
		fmt.Fprintln(w, "(nobody: every viewer plays from the origin)")
	}
	return w.Flush()
}

// assignViewer takes USER first, then --node, as pin does with SOURCE.
func assignViewer(svc *orchestrator.Service, assign bool, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("edge assign and unassign need a USER")
	}
	user := args[0]
	if !assign {
		if err := svc.UnassignViewer(user); err != nil {
			return err
		}
		fmt.Printf("%s plays from the origin now.\n", user)
		return nil
	}
	fs := flag.NewFlagSet("edge assign", flag.ExitOnError)
	node := fs.String("node", "", "edge node")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	n, err := svc.FindNode(*node)
	if err != nil {
		return err
	}
	from, err := svc.AssignViewer(user, n.ID)
	if err != nil {
		return err
	}
	if from != "" {
		fmt.Printf("%s moved from %s to %s. The router uses it within seconds.\n", user, from, n.Name)
	} else {
		fmt.Printf("%s now goes to %s. The router uses it within seconds.\n", user, n.Name)
	}
	return nil
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
	case "requeue":
		return requeue(svc, rest)
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

// requeue takes a running job from its worker and queues it again.
func requeue(svc *orchestrator.Service, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("queue requeue needs a JOB, then optionally --pause")
	}
	id := args[0]
	fs := flag.NewFlagSet("requeue", flag.ExitOnError)
	pause := fs.Bool("pause", false, "also pause the worker, so it takes no new job; a cloud server is drained and deleted")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	node, err := svc.Requeue(id, *pause)
	if err != nil {
		return err
	}
	fmt.Printf("Took %s from %s and queued it again. The worker stops at its next heartbeat; the attempt is not counted.\n", id, node)
	if *pause {
		fmt.Printf("%s is paused. Resume it with: orchestrator node resume %s\n", node, node)
	}
	return nil
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

func listJobs(store database.Store) error {
	jobs, err := store.List(100)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "JOB\tSTATE\tPRI\tTRY\tSTAGE\tPATH\tERROR")
	for _, j := range jobs {
		stage := j.Stage
		if j.State == model.StateRunning {
			stage = fmt.Sprintf("%s %.0f%%", j.Stage, j.Fraction*100)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\t%s\n", j.ID, j.State, j.Priority, j.Attempts, stage, j.Path, j.Error)
	}
	return w.Flush()
}
