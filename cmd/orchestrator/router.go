package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"jellyfinner/internal/config"
	"jellyfinner/internal/edgerouter"
	"jellyfinner/internal/jellyfin"
	"jellyfinner/internal/nodes"
)

// router runs in its own process, in front of Jellyfin, and sends direct
// streams of cached titles to the viewer's edge node. Keeping it apart from
// serve means restarting the orchestrator leaves Jellyfin reachable.
func router(envFile string) error {
	waitForFolders(envFile)
	cfg, err := config.Load(envFile)
	if err != nil {
		return err
	}
	if cfg.JellyfinURL == "" || cfg.JellyfinToken == "" {
		return errors.New("router needs JELLYFIN_URL and JELLYFIN_TOKEN")
	}
	upstream, err := url.Parse(cfg.JellyfinURL)
	if err != nil {
		return err
	}
	store, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	logger := log.New(os.Stderr, "", log.LstdFlags)
	registry := nodes.New(store, nil)
	for user, node := range cfg.EdgeViewers {
		if registry.Edge(node) == nil {
			logger.Printf("EDGE_VIEWERS sends %s to %s, which is not an edge node yet, so they stay on the origin", user, node)
		}
	}
	handler := edgerouter.NewRouter(cfg, store, jellyfin.New(cfg.JellyfinURL, cfg.JellyfinToken), upstream, logger)
	// No write timeout: viewers fall back to long streams through this proxy.
	server := &http.Server{Addr: cfg.RouterAddr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- server.ListenAndServe() }()
	logger.Printf("router on %s, in front of %s", cfg.RouterAddr, cfg.JellyfinURL)
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
