// Package config loads the orchestrator's settings from a .env file and the
// process environment. The environment wins, so a unit file can override.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"jellyfinner/internal/encode"
)

// Mode selects what starts an encode. Exactly one is active.
type Mode string

const (
	ModeAuto   Mode = "auto"
	ModeManual Mode = "manual"
)

// ParseMode has no default on purpose: guessing could queue a whole library.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeAuto, ModeManual:
		return Mode(s), nil
	case "":
		return "", errors.New("ENCODE_MODE is required: set it to auto or manual")
	}
	return "", fmt.Errorf("ENCODE_MODE must be auto or manual, got %q", s)
}

// Node is a worker or edge agent allowed on the control API. An empty Allow
// list accepts any source address.
type Node struct {
	Name  string
	Token string
	Allow []netip.Prefix
}

// Config holds every orchestrator setting.
type Config struct {
	Mode Mode

	DBPath       string
	OriginalsDir string
	LibraryDir   string
	IncomingDir  string

	// Rclone paths for the same directories as the worker sees them.
	RemoteOriginals string
	RemoteIncoming  string

	ControlAddr string
	TLSCert     string
	TLSKey      string
	Nodes       []Node

	ScanInterval      time.Duration
	StableFor         time.Duration
	DeleteGrace       time.Duration
	MassDeletePercent int
	LeaseFor          time.Duration
	RetryDelay        time.Duration
	MaxAttempts       int
	LibraryDebounce   time.Duration

	JellyfinURL   string
	JellyfinToken string
	// JellyfinLibraryDir is LIBRARY_DIR as Jellyfin sees it, when that differs.
	JellyfinLibraryDir string
	WebhookAddr        string
	WebhookToken       string

	// Output is the encode profile sent with every job.
	Output encode.Profile

	// TrustedProxies lists reverse proxies in front of the control API. Only
	// these may name the real client in X-Forwarded-For.
	TrustedProxies []netip.Prefix

	// BackfillDailyBytes caps how much source data backfill may start per UTC day.
	BackfillDailyBytes int64

	// EdgeNodes are the nodes that may cache encodes. They also need an entry in
	// NODE_TOKENS. EdgeViewers maps a Jellyfin username to its node.
	EdgeNodes    []EdgeNode
	EdgeViewers  map[string]string
	EdgeTokenKey string
	EdgeURLTTL   time.Duration
	RouterAddr   string

	// WebAddr enables the admin UI. MetricsAddr enables /metrics.
	WebAddr     string
	WebTLSCert  string
	WebTLSKey   string
	MetricsAddr string
	DiscordURL  string
	NtfyURL     string
}

// Load reads envFile, which may be absent, then validates the result.
func Load(envFile string) (*Config, error) {
	values, err := readEnvFile(envFile)
	if err != nil {
		return nil, err
	}
	get := func(key string) string {
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		return values[key]
	}
	return parse(get)
}

func parse(get func(string) string) (*Config, error) {
	mode, err := ParseMode(get("ENCODE_MODE"))
	if err != nil {
		return nil, err
	}
	nodes, err := ParseNodes(get("NODE_TOKENS"))
	if err != nil {
		return nil, err
	}
	c := &Config{
		Mode:               mode,
		DBPath:             get("DB_PATH"),
		OriginalsDir:       get("ORIGINALS_DIR"),
		LibraryDir:         get("LIBRARY_DIR"),
		IncomingDir:        get("INCOMING_DIR"),
		RemoteOriginals:    strings.TrimSuffix(get("REMOTE_ORIGINALS"), "/"),
		RemoteIncoming:     strings.TrimSuffix(get("REMOTE_INCOMING"), "/"),
		ControlAddr:        or(get("CONTROL_ADDR"), "127.0.0.1:8200"),
		TLSCert:            get("TLS_CERT"),
		TLSKey:             get("TLS_KEY"),
		Nodes:              nodes,
		JellyfinURL:        strings.TrimSuffix(get("JELLYFIN_URL"), "/"),
		JellyfinToken:      get("JELLYFIN_TOKEN"),
		JellyfinLibraryDir: get("JELLYFIN_LIBRARY_DIR"),
		WebhookAddr:        or(get("WEBHOOK_ADDR"), "127.0.0.1:8201"),
		WebhookToken:       get("WEBHOOK_TOKEN"),
		Output:             encode.DefaultProfile(),
		WebAddr:            get("WEB_ADDR"),
		WebTLSCert:         get("WEB_TLS_CERT"),
		WebTLSKey:          get("WEB_TLS_KEY"),
		MetricsAddr:        get("METRICS_ADDR"),
		EdgeTokenKey:       get("EDGE_TOKEN_KEY"),
		RouterAddr:         or(get("ROUTER_ADDR"), "127.0.0.1:8204"),
		DiscordURL:         get("NOTIFY_DISCORD_URL"),
		NtfyURL:            get("NOTIFY_NTFY_URL"),
	}
	for _, d := range []struct {
		key      string
		fallback time.Duration
		into     *time.Duration
	}{
		{"SCAN_INTERVAL", 5 * time.Minute, &c.ScanInterval},
		{"STABLE_FOR", 5 * time.Minute, &c.StableFor},
		{"DELETE_GRACE", 24 * time.Hour, &c.DeleteGrace},
		{"LEASE_FOR", 2 * time.Minute, &c.LeaseFor},
		{"RETRY_DELAY", time.Minute, &c.RetryDelay},
		{"LIBRARY_DEBOUNCE", 30 * time.Second, &c.LibraryDebounce},
		{"EDGE_URL_TTL", 24 * time.Hour, &c.EdgeURLTTL},
	} {
		v, err := duration(get(d.key), d.fallback)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.key, err)
		}
		*d.into = v
	}
	if c.MassDeletePercent, err = integer(get("MASS_DELETE_PERCENT"), 20); err != nil {
		return nil, fmt.Errorf("MASS_DELETE_PERCENT: %w", err)
	}
	if c.MaxAttempts, err = integer(get("MAX_ATTEMPTS"), 3); err != nil {
		return nil, fmt.Errorf("MAX_ATTEMPTS: %w", err)
	}
	for _, o := range []struct {
		key  string
		into *int
	}{
		{"OUTPUT_MAX_WIDTH", &c.Output.MaxWidth},
		{"OUTPUT_MAX_HEIGHT", &c.Output.MaxHeight},
		{"OUTPUT_VIDEO_KBPS", &c.Output.VideoKbps},
		{"OUTPUT_AUDIO_KBPS", &c.Output.AudioKbps},
		{"OUTPUT_KEYFRAME_SECS", &c.Output.KeyframeSecs},
	} {
		if *o.into, err = integer(get(o.key), *o.into); err != nil {
			return nil, fmt.Errorf("%s: %w", o.key, err)
		}
		if *o.into <= 0 {
			return nil, fmt.Errorf("%s must be positive", o.key)
		}
	}
	for _, lang := range strings.Split(get("OUTPUT_AUDIO_LANGUAGES"), ",") {
		if lang = strings.TrimSpace(lang); lang != "" {
			c.Output.Languages = append(c.Output.Languages, lang)
		}
	}
	if c.TrustedProxies, err = ParsePrefixes(get("TRUSTED_PROXIES")); err != nil {
		return nil, fmt.Errorf("TRUSTED_PROXIES: %w", err)
	}
	if c.EdgeNodes, err = ParseEdgeNodes(get("EDGE_NODES")); err != nil {
		return nil, err
	}
	if c.EdgeViewers, err = ParseEdgeViewers(get("EDGE_VIEWERS")); err != nil {
		return nil, err
	}
	dailyGB, err := integer(get("BACKFILL_DAILY_GB"), 200)
	if err != nil {
		return nil, fmt.Errorf("BACKFILL_DAILY_GB: %w", err)
	}
	if dailyGB <= 0 {
		return nil, errors.New("BACKFILL_DAILY_GB must be positive")
	}
	c.BackfillDailyBytes = int64(dailyGB) * 1e9
	return c, c.validate()
}

func (c *Config) validate() error {
	for _, req := range []struct{ key, value string }{
		{"DB_PATH", c.DBPath},
		{"ORIGINALS_DIR", c.OriginalsDir},
		{"LIBRARY_DIR", c.LibraryDir},
		{"INCOMING_DIR", c.IncomingDir},
	} {
		if req.value == "" {
			return fmt.Errorf("%s is required", req.key)
		}
	}
	// Only rclone workers use these, and then they go together.
	if (c.RemoteOriginals == "") != (c.RemoteIncoming == "") {
		return errors.New("set REMOTE_ORIGINALS and REMOTE_INCOMING together, or neither")
	}
	if c.Mode == ModeManual {
		if c.JellyfinURL == "" || c.JellyfinToken == "" {
			return errors.New("manual mode needs JELLYFIN_URL and JELLYFIN_TOKEN, to look up what was played")
		}
		if len(c.WebhookToken) < 32 {
			return errors.New("manual mode needs WEBHOOK_TOKEN of at least 32 characters")
		}
	}
	if err := c.validateEdge(); err != nil {
		return err
	}
	if c.WebAddr != "" && (c.JellyfinURL == "") {
		return errors.New("WEB_ADDR needs JELLYFIN_URL, because login goes through Jellyfin")
	}
	if (c.WebTLSCert == "") != (c.WebTLSKey == "") {
		return errors.New("WEB_TLS_CERT and WEB_TLS_KEY must be set together")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("TLS_CERT and TLS_KEY must be set together")
	}
	if c.MaxAttempts < 1 {
		return errors.New("MAX_ATTEMPTS must be at least 1")
	}
	if c.MassDeletePercent < 0 || c.MassDeletePercent > 100 {
		return errors.New("MASS_DELETE_PERCENT must be between 0 and 100")
	}
	if nested(c.LibraryDir, c.OriginalsDir) || nested(c.OriginalsDir, c.LibraryDir) {
		return errors.New("ORIGINALS_DIR and LIBRARY_DIR must not contain each other")
	}
	return nil
}

func (c *Config) validateEdge() error {
	if len(c.EdgeNodes) == 0 {
		return nil
	}
	if len(c.EdgeTokenKey) < 32 {
		return errors.New("EDGE_NODES needs EDGE_TOKEN_KEY of at least 32 characters")
	}
	for _, n := range c.EdgeNodes {
		found := false
		for _, node := range c.Nodes {
			found = found || node.Name == n.Name
		}
		if !found {
			return fmt.Errorf("edge node %q needs a token in NODE_TOKENS", n.Name)
		}
	}
	for user, node := range c.EdgeViewers {
		if c.EdgeNode(node) == nil {
			return fmt.Errorf("EDGE_VIEWERS: %s is assigned to unknown node %q", user, node)
		}
	}
	return nil
}

// EdgeNode finds an edge node by name.
func (c *Config) EdgeNode(name string) *EdgeNode {
	for i := range c.EdgeNodes {
		if c.EdgeNodes[i].Name == name {
			return &c.EdgeNodes[i]
		}
	}
	return nil
}

// EdgeNode is a small file server near some viewers.
type EdgeNode struct {
	Name       string
	URL        string
	QuotaBytes int64
	SigningKey string
}

// ParseEdgeNodes reads "name=https://host,quotaGB,signingkey;...".
func ParseEdgeNodes(s string) ([]EdgeNode, error) {
	var nodes []EdgeNode
	for _, entry := range strings.Split(s, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, rest, ok := strings.Cut(entry, "=")
		parts := strings.Split(rest, ",")
		if !ok || name == "" || len(parts) != 3 {
			return nil, fmt.Errorf("EDGE_NODES: entry for %q must be name=url,quotaGB,key", name)
		}
		u, err := url.Parse(parts[0])
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("EDGE_NODES: node %q needs an https URL", name)
		}
		quota, err := strconv.Atoi(parts[1])
		if err != nil || quota <= 0 {
			return nil, fmt.Errorf("EDGE_NODES: node %q needs a positive quota in GB", name)
		}
		if len(parts[2]) < 32 {
			return nil, fmt.Errorf("EDGE_NODES: signing key for %q must be at least 32 characters", name)
		}
		nodes = append(nodes, EdgeNode{Name: name, URL: parts[0], QuotaBytes: int64(quota) * 1e9, SigningKey: parts[2]})
	}
	return nodes, nil
}

// ParseEdgeViewers reads "username=node;...".
func ParseEdgeViewers(s string) (map[string]string, error) {
	viewers := map[string]string{}
	for _, entry := range strings.Split(s, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		user, node, ok := strings.Cut(entry, "=")
		if !ok || user == "" || node == "" {
			return nil, fmt.Errorf("EDGE_VIEWERS: entry %q must be username=node", entry)
		}
		viewers[user] = node
	}
	return viewers, nil
}

func nested(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// CheckFilesystems confirms publishing can be atomic, by doing a real rename.
// Comparing device IDs is not enough: two container mounts of one disk share a
// device, yet a rename between them fails.
func (c *Config) CheckFilesystems() error {
	const name = ".jellyfinner-probe"
	src := filepath.Join(c.IncomingDir, name)
	dst := filepath.Join(c.LibraryDir, name)
	if err := os.WriteFile(src, nil, 0o600); err != nil {
		return fmt.Errorf("INCOMING_DIR: %w", err)
	}
	defer os.Remove(src)
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("INCOMING_DIR and LIBRARY_DIR must be on one filesystem and, in a container, under one mount: %w", err)
	}
	return os.Remove(dst)
}

// ClientAddr finds the real client of a request. X-Forwarded-For counts only
// when the connection comes from a trusted proxy, and only its last entry,
// because that is the one the proxy added. Earlier entries come from the client.
func (c *Config) ClientAddr(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	addr = addr.Unmap()
	if !slices.ContainsFunc(c.TrustedProxies, func(p netip.Prefix) bool { return p.Contains(addr) }) {
		return addr, true
	}
	values := r.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return addr, true
	}
	hops := strings.Split(values[len(values)-1], ",")
	client, err := netip.ParseAddr(strings.TrimSpace(hops[len(hops)-1]))
	if err != nil {
		return netip.Addr{}, false
	}
	return client.Unmap(), true
}

// ParsePrefixes reads a comma-separated list of addresses or CIDR ranges.
func ParsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !strings.Contains(entry, "/") {
			addr, err := netip.ParseAddr(entry)
			if err != nil {
				return nil, err
			}
			out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			return nil, err
		}
		out = append(out, prefix)
	}
	return out, nil
}

// ParseNodes reads "name=token@10.0.0.2/32+10.0.1.0/24;other=token". The
// allowlist after @ is optional.
func ParseNodes(s string) ([]Node, error) {
	var nodes []Node
	seen := map[string]bool{}
	for _, entry := range strings.Split(s, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, rest, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("NODE_TOKENS: entry %q must be name=token", name)
		}
		token, allow, _ := strings.Cut(rest, "@")
		if len(token) < 32 {
			return nil, fmt.Errorf("NODE_TOKENS: token for %q must be at least 32 characters", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("NODE_TOKENS: duplicate node %q", name)
		}
		seen[name] = true
		node := Node{Name: name, Token: token}
		if allow != "" {
			for _, cidr := range strings.Split(allow, "+") {
				prefix, err := netip.ParsePrefix(cidr)
				if err != nil {
					return nil, fmt.Errorf("NODE_TOKENS: node %q: %w", name, err)
				}
				node.Allow = append(node.Allow, prefix)
			}
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// CheckControlURL enforces the transport rule: HTTPS anywhere, plain HTTP only
// to a private address, which in practice is a WireGuard peer.
func CheckControlURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("control URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		addr, err := netip.ParseAddr(u.Hostname())
		if err != nil {
			return fmt.Errorf("control URL %q: plain http needs a private IP address, not a hostname", raw)
		}
		if !addr.IsPrivate() && !addr.IsLoopback() {
			return fmt.Errorf("control URL %q: plain http is only allowed to a private address", raw)
		}
		return nil
	}
	return fmt.Errorf("control URL %q: scheme must be https or http", raw)
}

func readEnvFile(path string) (map[string]string, error) {
	values := map[string]string{}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return values, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		values[strings.TrimSpace(key)] = unquote(strings.TrimSpace(value))
	}
	return values, scanner.Err()
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func duration(v string, fallback time.Duration) (time.Duration, error) {
	if v == "" {
		return fallback, nil
	}
	return time.ParseDuration(v)
}

func integer(v string, fallback int) (int, error) {
	if v == "" {
		return fallback, nil
	}
	return strconv.Atoi(v)
}
