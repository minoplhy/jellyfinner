// Package edgerouter is the reverse proxy in front of Jellyfin.
package edgerouter

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"jellyfinner/internal/config"
	"jellyfinner/internal/database"
	"jellyfinner/internal/edge"
	"jellyfinner/internal/jellyfin"
	"jellyfinner/internal/model"
	"jellyfinner/internal/nodes"
)

const (
	userCacheFor = time.Minute
	itemCacheFor = 5 * time.Minute
)

// streamPath matches Jellyfin's direct-stream URLs, with or without a media
// source segment, and in either case.
var streamPath = regexp.MustCompile(`(?i)^/videos/([0-9a-f-]{32,36})(?:/[^/]+)?/stream(?:\.[a-z0-9]+)?$`)

// tokenInHeader finds Token="..." in a MediaBrowser Authorization header.
var tokenInHeader = regexp.MustCompile(`(?i)\bToken="([^"]+)"`)

type cached struct {
	value string
	until time.Time
}

// Router sits in front of Jellyfin. Everything passes straight through, except
// a direct stream of a title cached near the viewer, which gets a 302. Every
// doubt or error also passes through, so a router fault can only ever mean
// streaming from the origin.
type Router struct {
	cfg      *config.Config
	store    database.Store
	nodes    *nodes.Registry
	jellyfin jellyfin.API
	proxy    http.Handler
	base     string // Jellyfin's Base URL, such as /jf
	log      *log.Logger
	// Now is the clock, replaceable in tests.
	Now func() time.Time

	mu    sync.Mutex
	users map[string]cached
	items map[string]cached
}

// NewRouter returns a router in front of upstream. A path in upstream is
// Jellyfin's Base URL. Requests pass with their path unchanged, because
// Jellyfin expects that prefix itself.
func NewRouter(cfg *config.Config, store database.Store, jf jellyfin.API, upstream *url.URL, logger *log.Logger) *Router {
	base, _ := config.BasePath(upstream.Path)
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = upstream.Scheme, upstream.Host
			// Rewrite drops forwarded headers. An outer proxy's must reach
			// Jellyfin, which builds URLs from X-Forwarded-Proto, so they are
			// kept and this hop only adds itself to X-Forwarded-For.
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				if v, ok := pr.In.Header[h]; ok {
					pr.Out.Header[h] = v
				}
			}
			if ip, _, err := net.SplitHostPort(pr.In.RemoteAddr); err == nil {
				if prior := pr.Out.Header["X-Forwarded-For"]; len(prior) > 0 {
					ip = strings.Join(prior, ", ") + ", " + ip
				}
				pr.Out.Header.Set("X-Forwarded-For", ip)
			}
			if _, ok := pr.Out.Header["User-Agent"]; !ok {
				pr.Out.Header.Set("User-Agent", "")
			}
		},
	}
	rt := &Router{cfg: cfg, store: store, jellyfin: jf, proxy: proxy, base: base, log: logger, Now: time.Now,
		users: map[string]cached{}, items: map[string]cached{}}
	rt.nodes = nodes.New(store, func() time.Time { return rt.Now() })
	return rt
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if target := rt.redirectFor(r); target != "" {
		// The URL is signed and expires, so it must not be stored.
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	rt.proxy.ServeHTTP(w, r)
}

func (rt *Router) redirectFor(r *http.Request) string {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return ""
	}
	// With or without Jellyfin's Base URL, so a proxy may strip it or not.
	m := streamPath.FindStringSubmatch(config.TrimPrefix(r.URL.Path, rt.base))
	if m == nil || !isStatic(r.URL.Query()) {
		return ""
	}
	token := requestToken(r)
	if token == "" {
		return ""
	}
	target, err := rt.resolve(r.Context(), m[1], token)
	if err != nil {
		rt.log.Printf("router, passing through: %v", err)
		return ""
	}
	return target
}

func isStatic(q url.Values) bool {
	for key, values := range q {
		if strings.EqualFold(key, "static") && len(values) > 0 && strings.EqualFold(values[0], "true") {
			return true
		}
	}
	return false
}

func requestToken(r *http.Request) string {
	for key, values := range r.URL.Query() {
		if (strings.EqualFold(key, "api_key") || strings.EqualFold(key, "apikey")) && len(values) > 0 {
			return values[0]
		}
	}
	for _, header := range []string{"X-Emby-Token", "X-MediaBrowser-Token"} {
		if v := r.Header.Get(header); v != "" {
			return v
		}
	}
	for _, header := range []string{"Authorization", "X-Emby-Authorization"} {
		if m := tokenInHeader.FindStringSubmatch(r.Header.Get(header)); m != nil {
			return m[1]
		}
	}
	return ""
}

func (rt *Router) resolve(ctx context.Context, itemID, token string) (string, error) {
	user, err := rt.lookup(rt.users, token, userCacheFor, func() (string, error) {
		return rt.jellyfin.Username(ctx, token)
	})
	if err != nil {
		return "", err
	}
	node := rt.nodes.Edge(rt.cfg.EdgeViewers[user])
	if node == nil {
		return "", nil
	}
	itemPath, err := rt.lookup(rt.items, itemID, itemCacheFor, func() (string, error) {
		return rt.jellyfin.ItemPath(ctx, itemID)
	})
	if err != nil {
		return "", err
	}
	output, ok := relative(rt.libraryRoot(), itemPath)
	if !ok {
		return "", nil
	}
	p, err := rt.store.ActivePlacement(output, node.Name)
	if errors.Is(err, model.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	status, err := rt.store.GetNodeStatus(node.Name)
	if err != nil {
		return "", err
	}
	now := rt.Now()
	if !Servable(status, now) {
		return "", nil
	}
	if err := rt.store.TouchPlacement(node.Name, p.SHA256); err != nil {
		rt.log.Printf("router, record play: %v", err)
	}
	return edge.SignURL(node.URL, node.SigningKey, p.SHA256, now.Add(rt.cfg.EdgeURLTTL)), nil
}

// HealthWindow is how recently an agent must have reported for its node to be
// used.
const HealthWindow = time.Minute

// Servable is the health rule: the orchestrator's probe passed and the agent reported
// recently.
func Servable(st model.NodeStatus, now time.Time) bool {
	return st.Healthy && now.Sub(time.Unix(st.LastSeen, 0)) < HealthWindow
}

func (rt *Router) libraryRoot() string {
	if rt.cfg.JellyfinLibraryDir != "" {
		return rt.cfg.JellyfinLibraryDir
	}
	return rt.cfg.LibraryDir
}

func relative(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func (rt *Router) lookup(cache map[string]cached, key string, ttl time.Duration, fetch func() (string, error)) (string, error) {
	now := rt.Now()
	rt.mu.Lock()
	entry, ok := cache[key]
	if ok && now.After(entry.until) {
		delete(cache, key)
		ok = false
	}
	rt.mu.Unlock()
	if ok {
		return entry.value, nil
	}
	value, err := fetch()
	if err != nil {
		return "", err
	}
	rt.mu.Lock()
	cache[key] = cached{value: value, until: now.Add(ttl)}
	rt.mu.Unlock()
	return value, nil
}
