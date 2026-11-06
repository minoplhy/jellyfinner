// Command edge-agent runs on an edge node. Build it with CGO_ENABLED=0, so one
// static binary serves glibc and Alpine hosts.
//
// It has two subcommands, run as two processes under two users. pull writes
// to the directory and connects out, with no listener. serve only reads it,
// and answers viewers behind a reverse proxy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"jellyfinner/internal/buildinfo"
	"jellyfinner/internal/control"
	"jellyfinner/internal/doctor"
	"jellyfinner/internal/edgeagent"
	"jellyfinner/internal/prepare"
)

const usage = `usage: edge-agent pull [--url URL] [--dir DIR] [--poll D] [--limit-mb N]
       edge-agent serve [--addr ADDR] [--dir DIR]

       edge-agent health pull | health serve [ADDR]
       edge-agent doctor [--dir DIR]
       edge-agent prepare --owner UID:GID PATH[:MODE]...
       edge-agent version

pull reads NODE_TOKEN, and serve reads EDGE_SIGNING_KEY.
`

const healthMaxAge = 3 * time.Minute

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "pull":
		err = pull(ctx, os.Args[2:])
	case "serve":
		err = serve(ctx, os.Args[2:])
	case "health":
		err = health(os.Args[2:])
	case "doctor":
		err = runDoctor(ctx, os.Args[2:])
	case "prepare":
		err = prepare.Command(os.Args[2:])
	case "version":
		fmt.Println(buildinfo.Line("edge-agent"))
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "edge-agent:", err)
		os.Exit(1)
	}
}

// runDoctor checks whichever half of the node this container is. Each one holds
// only its own secret, so that says which it is.
func runDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	dir := fs.String("dir", "/edge", "directory files are stored in")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := doctor.New(os.Stdout)
	key := os.Getenv("EDGE_SIGNING_KEY")
	token := os.Getenv("NODE_TOKEN")
	if key == "" && token == "" {
		r.Check("settings", errors.New("neither NODE_TOKEN nor EDGE_SIGNING_KEY is set"))
	}
	if key != "" {
		r.Check("files readable at "+*dir, doctor.Readable(*dir))
	}
	if token != "" {
		r.Check("files writable at "+*dir, doctor.Writable(*dir))
		r.Check("free space", doctor.FreeSpace(*dir, 5))
		client, err := control.NewClient(os.Getenv("ORCHESTRATOR_URL"), token)
		r.Check("orchestrator URL", err)
		if err == nil {
			client.Role = "edge-agent"
			checkNode(ctx, r, client)
		}
	}
	return r.Done()
}

func checkNode(ctx context.Context, r *doctor.Report, client control.API) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	me, err := client.WhoAmI(ctx)
	r.Check("orchestrator reachable and token accepted", err)
	if err != nil {
		return
	}
	if me.Kind != "edge" {
		r.Check("node kind", fmt.Errorf("%s is a %s, add it with --edge", me.Name, me.Kind))
		return
	}
	r.Note("this node is " + me.Name)
}

// health is for container health checks. The image has no shell or curl.
func health(args []string) error {
	if len(args) >= 1 && args[0] == "pull" {
		return control.CheckAlive(healthMaxAge)
	}
	if len(args) == 0 || args[0] != "serve" {
		return errors.New("health needs pull or serve")
	}
	addr := "127.0.0.1:8205"
	if len(args) > 1 {
		addr = args[1]
	}
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return nil
}

func pull(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pull", flag.ExitOnError)
	url := fs.String("url", os.Getenv("ORCHESTRATOR_URL"), "orchestrator control URL")
	dir := fs.String("dir", "/edge", "directory files are stored in")
	poll := fs.Duration("poll", 15*time.Second, "how often to report")
	limitMB := fs.Int64("limit-mb", 20, "pull cap in MB per second, 0 for none")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *limitMB < 0 {
		return errors.New("--limit-mb cannot be negative")
	}
	prepare.Wait(prepare.WaitFor, *dir)
	client, err := control.NewClient(*url, os.Getenv("NODE_TOKEN"))
	if err != nil {
		return err
	}
	client.Role = "edge-agent"
	log.Print(buildinfo.Line("edge-agent"))
	agent := &edgeagent.Agent{Client: client, Dir: *dir, Poll: *poll, Limit: *limitMB * 1e6}
	return agent.Run(ctx)
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8205", "address the reverse proxy connects to")
	dir := fs.String("dir", "/edge", "directory files are stored in")
	if err := fs.Parse(args); err != nil {
		return err
	}
	key := os.Getenv("EDGE_SIGNING_KEY")
	if key == "" {
		return errors.New("EDGE_SIGNING_KEY is required")
	}
	// No write timeout: a stream lasts as long as the title.
	server := &http.Server{
		Addr:              *addr,
		Handler:           &edgeagent.FileServer{Dir: *dir, Key: key},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- server.ListenAndServe() }()
	log.Printf("%s: serving %s on %s", buildinfo.Line("edge-agent"), *dir, *addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}
