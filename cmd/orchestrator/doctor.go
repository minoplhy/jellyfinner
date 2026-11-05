package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"jellyfinner/internal/config"
	"jellyfinner/internal/database"
	"jellyfinner/internal/doctor"
	"jellyfinner/internal/jellyfin"
	"jellyfinner/internal/model"
	"jellyfinner/internal/notify"
	"jellyfinner/internal/orchestrator"
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
	if entries, err := os.ReadDir(cfg.OriginalsDir); err == nil && len(entries) == 0 {
		r.Note("originals folder is empty: check the mount")
	}
	r.Check("library writable at "+cfg.LibraryDir, doctor.Writable(cfg.LibraryDir))
	r.Check("incoming writable at "+cfg.IncomingDir, doctor.Writable(cfg.IncomingDir))
	r.Check("library and incoming allow an atomic publish", cfg.CheckFilesystems())
	r.Check("free space for encodes", doctor.FreeSpace(cfg.LibraryDir, minFreeGB))
	if cfg.TLSCert == "" {
		r.Check("control API address", config.CheckListenAddr(cfg.ControlAddr))
	}

	store, err := openStore(cfg)
	r.Check("database at "+cfg.DBPath, err)
	if err == nil {
		defer store.Close()
		r.Check("database mode matches ENCODE_MODE", orchestrator.CheckMode(store, string(cfg.Mode)))
		nodes, err := store.LoadNodes()
		r.Check("node list", err)
		if err == nil && len(nodes) == 0 {
			r.Note("no nodes yet: add one with orchestrator node add NAME")
		}
		if cfg.LegacyEdgeViewers {
			r.Note(orchestrator.LegacyViewersNote)
		}
		checkCloud(r, cfg, store)
	}

	if webhookUnreachable(cfg) {
		r.Note("WEBHOOK_ADDR is " + cfg.WebhookAddr + ", which only this container can reach, so Jellyfin cannot report plays")
	}
	if cfg.ControlURL == "" {
		r.Note("CONTROL_URL is not set, so node add cannot print the address nodes need")
	}
	adminNotes(r, cfg)
	checkJellyfin(r, cfg)
	return r.Done()
}

// checkCloud validates each listed provider against its API, and fails while
// any rented server's delete is unconfirmed, because it may still be billed.
func checkCloud(r *doctor.Report, cfg *config.Config, store database.Store) {
	svc := orchestrator.New(cfg, store, notify.New("", ""), nil, log.New(io.Discard, "", 0))
	r.Check("every provider with a tracked server is still listed", orchestrator.CheckCloudProviders(svc))
	rows, err := store.CloudServers(time.Now())
	r.Check("cloud server list", err)
	for _, row := range rows {
		if row.State == model.CloudDeleting && time.Since(time.Unix(row.DeleteAt, 0)) >= cfg.Cloud.DeleteTimeout {
			r.Check("cloud server "+row.Name, fmt.Errorf("deleting %s server %s is not confirmed and it may still be billed: check the %s console",
				row.Provider, row.ID, row.Provider))
		}
	}
	if !cfg.Cloud.Enabled() {
		return
	}
	providers, err := orchestrator.BuildProviders(cfg.Cloud)
	r.Check("cloud provider settings", err)
	if err != nil {
		return
	}
	for _, p := range providers {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		r.Check(p.Name()+": token, project, types, locations and image", p.Check(ctx))
		cancel()
	}
	r.Note("cloud providers are tried in this order: " + strings.Join(providerNames(providers), ", "))
}

// adminNotes points out admin UI settings that work but are likely wrong.
func adminNotes(r *doctor.Report, cfg *config.Config) {
	if cfg.AdminAddr == "" {
		return
	}
	if cfg.AdminURL == "" {
		r.Note("ADMIN_URL is not set, so the admin UI is served at / and its cookie is only marked secure behind https")
		return
	}
	if sameSite(cfg.AdminURL, cfg.ControlURL) && cfg.AdminBasePath() == cfg.ControlBasePath() {
		r.Note("ADMIN_URL and CONTROL_URL share a host and path, so a proxy cannot tell them apart: give one a path")
	}
}

// sameSite reports whether two URLs name the same host and port.
func sameSite(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	return errA == nil && errB == nil && ua.Host != "" && strings.EqualFold(ua.Host, ub.Host)
}

// webhookUnreachable spots a manual-mode listener on the container's own
// loopback. On a host install, a Jellyfin beside it can still reach that.
func webhookUnreachable(cfg *config.Config) bool {
	if cfg.Mode != config.ModeManual || os.Getenv("JELLYFINNER_IN_CONTAINER") != "1" {
		return false
	}
	host, _, err := net.SplitHostPort(cfg.WebhookAddr)
	if err != nil {
		return false
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
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
