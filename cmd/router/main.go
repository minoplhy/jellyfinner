// Command router runs next to Jellyfin, in front of it, and sends direct
// streams of titles cached on an edge node to that node. It asks the
// orchestrator where each viewer goes, over the control API with its own node
// token, and holds no database and no edge secret. Build it with
// CGO_ENABLED=0.
//
// Everything else, and every doubt, passes through to Jellyfin, so with the
// orchestrator down viewers simply stream from the origin.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"jellyfinner/internal/control"
	"jellyfinner/internal/doctor"
	"jellyfinner/internal/edgerouter"
	"jellyfinner/internal/jellyfin"
	"jellyfinner/internal/model"
)

const usage = `usage: router [serve]
       router health
       router doctor

Settings come from the environment:
  ORCHESTRATOR_URL      the orchestrator's CONTROL_URL
  NODE_TOKEN            printed by orchestrator node add NAME --router
  JELLYFIN_URL          Jellyfin as this router reaches it, with any Base URL
  JELLYFIN_TOKEN        a Jellyfin API key
  JELLYFIN_LIBRARY_DIR  the library folder as Jellyfin sees it
  ROUTER_ADDR           where to listen, 127.0.0.1:8204 by default
`

// beat is how often an idle router tells the orchestrator it is alive, so the
// Nodes page can show it online.
const beat = time.Minute

func main() {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch command {
	case "serve":
		err = serve(ctx)
	case "health":
		err = health()
	case "doctor":
		err = runDoctor(ctx)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "router:", err)
		os.Exit(1)
	}
}

// settings is the router's whole configuration.
type settings struct {
	orchestratorURL string
	token           string
	jellyfinURL     string
	jellyfinToken   string
	libraryDir      string
	addr            string
}

// load reads the environment and names every missing setting at once.
func load(getenv func(string) string) (settings, error) {
	s := settings{
		orchestratorURL: getenv("ORCHESTRATOR_URL"),
		token:           getenv("NODE_TOKEN"),
		jellyfinURL:     strings.TrimSuffix(getenv("JELLYFIN_URL"), "/"),
		jellyfinToken:   getenv("JELLYFIN_TOKEN"),
		libraryDir:      strings.TrimSuffix(getenv("JELLYFIN_LIBRARY_DIR"), "/"),
		addr:            getenv("ROUTER_ADDR"),
	}
	if s.addr == "" {
		s.addr = "127.0.0.1:8204"
	}
	var missing []string
	for name, v := range map[string]string{"ORCHESTRATOR_URL": s.orchestratorURL, "NODE_TOKEN": s.token,
		"JELLYFIN_URL": s.jellyfinURL, "JELLYFIN_TOKEN": s.jellyfinToken, "JELLYFIN_LIBRARY_DIR": s.libraryDir} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return s, fmt.Errorf("%s not set", strings.Join(missing, ", "))
	}
	if !strings.HasPrefix(s.libraryDir, "/") {
		return s, fmt.Errorf("JELLYFIN_LIBRARY_DIR must be an absolute path, as Jellyfin sees it: %q", s.libraryDir)
	}
	u, err := url.Parse(s.jellyfinURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return s, fmt.Errorf("JELLYFIN_URL %q must be an http or https address", s.jellyfinURL)
	}
	return s, nil
}

func serve(ctx context.Context) error {
	s, err := load(os.Getenv)
	if err != nil {
		return err
	}
	client, err := control.NewClient(s.orchestratorURL, s.token)
	if err != nil {
		return err
	}
	upstream, err := url.Parse(s.jellyfinURL)
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "", log.LstdFlags)
	handler := edgerouter.NewRouter(edgerouter.Settings{LibraryDir: s.libraryDir}, client,
		jellyfin.New(s.jellyfinURL, s.jellyfinToken), upstream, logger)
	// No write timeout: viewers on the origin stream through this proxy.
	server := &http.Server{Addr: s.addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	go heartbeat(ctx, client, logger)
	errc := make(chan error, 1)
	go func() { errc <- server.ListenAndServe() }()
	logger.Printf("router on %s, in front of %s", s.addr, s.jellyfinURL)
	select {
	case err = <-errc:
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = server.Shutdown(shutdown)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// heartbeat checks in now and then. A failure is logged once, and once more
// when it recovers: viewers stream from the origin meanwhile.
func heartbeat(ctx context.Context, client *control.Client, logger *log.Logger) {
	failing := false
	for {
		call, cancel := context.WithTimeout(ctx, 15*time.Second)
		me, err := client.WhoAmI(call)
		cancel()
		switch {
		case err != nil && !failing:
			logger.Printf("orchestrator unreachable, so every viewer streams from the origin: %v", err)
			failing = true
		case err == nil && me.Kind != model.NodeRouter:
			logger.Printf("this token belongs to %s, a %s: add a router with orchestrator node add NAME --router", me.Name, me.Kind)
			failing = true
		case err == nil && failing:
			logger.Printf("orchestrator reachable again, as %s", me.Name)
			failing = false
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(beat):
		}
	}
}

// health is for container health checks. The image has no shell or curl.
func health() error {
	addr := os.Getenv("ROUTER_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8204"
	}
	if host, port, err := net.SplitHostPort(addr); err == nil && (host == "" || host == "0.0.0.0" || host == "::") {
		addr = net.JoinHostPort("127.0.0.1", port)
	}
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

func runDoctor(ctx context.Context) error {
	r := doctor.New(os.Stdout)
	s, err := load(os.Getenv)
	r.Check("settings", err)
	if err != nil {
		return r.Done()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client, err := control.NewClient(s.orchestratorURL, s.token)
	r.Check("orchestrator URL", err)
	if err == nil {
		me, err := client.WhoAmI(ctx)
		r.Check("orchestrator reachable and token accepted", err)
		if err == nil && me.Kind != model.NodeRouter {
			r.Check("node kind", fmt.Errorf("%s is a %s, add a router with orchestrator node add NAME --router", me.Name, me.Kind))
		} else if err == nil {
			r.Note("this router is " + me.Name)
		}
	}
	r.Check("Jellyfin at "+s.jellyfinURL+" with JELLYFIN_TOKEN", jellyfin.New(s.jellyfinURL, s.jellyfinToken).Check(ctx))
	r.Note("JELLYFIN_LIBRARY_DIR is " + s.libraryDir + ": it must be the library folder as Jellyfin sees it, or nobody is redirected")
	return r.Done()
}
