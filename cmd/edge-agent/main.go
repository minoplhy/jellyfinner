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

	"jellyfinner/internal/control"
	"jellyfinner/internal/edgeagent"
)

const usage = `usage: edge-agent pull [--url URL] [--dir DIR] [--poll D] [--limit-mb N]
       edge-agent serve [--addr ADDR] [--dir DIR]

       edge-agent health pull | health serve [ADDR]

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
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "edge-agent:", err)
		os.Exit(1)
	}
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
	client, err := control.NewClient(*url, os.Getenv("NODE_TOKEN"))
	if err != nil {
		return err
	}
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
	log.Printf("serving %s on %s", *dir, *addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}
