// Package edgerouter is the reverse proxy in front of Jellyfin.
package edgerouter

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"jellyfinner/internal/config"
	"jellyfinner/internal/edge"
	"jellyfinner/internal/queue"
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

// Jellyfin is what the router asks of the media server.
type Jellyfin interface {
	ItemPath(ctx context.Context, itemID string) (string, error)
	Username(ctx context.Context, token string) (string, error)
}

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
	store    *queue.Store
	jellyfin Jellyfin
	proxy    http.Handler
	log      *log.Logger

	mu    sync.Mutex
	users map[string]cached
	items map[string]cached
}

// NewRouter returns a router in front of upstream.
func NewRouter(cfg *config.Config, store *queue.Store, jf Jellyfin, upstream *url.URL, logger *log.Logger) *Router {
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.FlushInterval = -1
	return &Router{cfg: cfg, store: store, jellyfin: jf, proxy: proxy, log: logger, users: map[string]cached{}, items: map[string]cached{}}
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
	m := streamPath.FindStringSubmatch(r.URL.Path)
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
	node := rt.store.EdgeNode(rt.cfg.EdgeViewers[user])
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
	if errors.Is(err, queue.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	status, err := rt.store.GetNodeStatus(node.Name)
	if err != nil {
		return "", err
	}
	now := rt.store.Now()
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

// Servable is the health rule: the probe from B passed and the agent reported
// recently.
func Servable(st queue.NodeStatus, now time.Time) bool {
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
	now := rt.store.Now()
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
