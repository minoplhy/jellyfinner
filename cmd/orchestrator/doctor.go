package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"jellyfinner/internal/config"
	"jellyfinner/internal/doctor"
	"jellyfinner/internal/jellyfin"
	"jellyfinner/internal/queue"
)

const minFreeGB = 20

// runDoctor keeps going after a failure, so one run shows everything wrong.
func runDoctor(envFile string) error {
	r := doctor.New(os.Stdout)
	cfg, err := config.Load(envFile)
	r.Check("settings", err)
	if err != nil {
		return r.Done()
	}
	r.Check("originals readable at "+cfg.OriginalsDir, doctor.Readable(cfg.OriginalsDir))
	r.Check("library writable at "+cfg.LibraryDir, doctor.Writable(cfg.LibraryDir))
	r.Check("incoming writable at "+cfg.IncomingDir, doctor.Writable(cfg.IncomingDir))
	r.Check("library and incoming allow an atomic publish", cfg.CheckFilesystems())
	r.Check("free space for encodes", doctor.FreeSpace(cfg.LibraryDir, minFreeGB))
	if cfg.TLSCert == "" {
		r.Check("control API address", config.CheckListenAddr(cfg.ControlAddr))
	}

	store, err := queue.Open(cfg.DBPath)
	r.Check("database at "+cfg.DBPath, err)
	if err == nil {
		defer store.Close()
		r.Check("database mode matches ENCODE_MODE", store.CheckMode(string(cfg.Mode)))
		nodes, err := store.LoadNodes()
		r.Check("node list", err)
		if err == nil && len(nodes) == 0 {
			r.Note("no nodes yet: add one with orchestrator node add NAME")
		}
		viewerNodes(r, cfg, nodes)
	}

	if cfg.ControlURL == "" {
		r.Note("CONTROL_URL is not set, so node add cannot print the address nodes need")
	}
	checkJellyfin(r, cfg)
	return r.Done()
}

func viewerNodes(r *doctor.Report, cfg *config.Config, nodes []queue.Node) {
	known := map[string]bool{}
	for _, n := range nodes {
		known[n.Name] = n.Edge()
	}
	for user, node := range cfg.EdgeViewers {
		if !known[node] {
			r.Note(fmt.Sprintf("EDGE_VIEWERS sends %s to %s, which is not an edge node, so they stay on the origin", user, node))
		}
	}
}

func checkJellyfin(r *doctor.Report, cfg *config.Config) {
	if cfg.JellyfinURL == "" {
		r.Note("JELLYFIN_URL is not set, so Jellyfin finds new titles on its own schedule")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if cfg.JellyfinToken == "" {
		r.Note("JELLYFIN_TOKEN is not set, so Jellyfin is not told about new titles")
		return
	}
	r.Check("Jellyfin at "+cfg.JellyfinURL, jellyfin.New(cfg.JellyfinURL, cfg.JellyfinToken).Check(ctx))
}
