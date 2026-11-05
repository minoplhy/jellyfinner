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
	"regexp"
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
	// ControlURL is how nodes reach the control API. node add prints it, and
	// any path in it is a prefix the control API also answers under.
	ControlURL string
	TLSCert    string
	TLSKey     string

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

	// EdgeViewers maps a Jellyfin username to its node. EdgeTokenKey comes from
	// the database, not the environment.
	EdgeViewers  map[string]string
	EdgeTokenKey string
	EdgeURLTTL   time.Duration
	RouterAddr   string

	// AdminAddr enables the admin UI. AdminURL is where people open it, and
	// its path, such as /jellyfinner-admin, is where the UI is served.
	// MetricsAddr enables /metrics.
	AdminAddr    string
	AdminURL     string
	AdminTLSCert string
	AdminTLSKey  string
	MetricsAddr  string
	DiscordURL   string
	NtfyURL      string
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

// renamed lists settings that were removed. Starting with one set fails, so an
// upgrade cannot quietly switch the admin UI off.
var renamed = []struct{ old, hint string }{
	{"WEB_ADDR", "set ADMIN_ADDR instead"},
	{"WEB_BASE_PATH", "set ADMIN_URL to the full address, such as https://example.com/jellyfinner-admin"},
	{"WEB_TLS_CERT", "set ADMIN_TLS_CERT instead"},
	{"WEB_TLS_KEY", "set ADMIN_TLS_KEY instead"},
}

func parse(get func(string) string) (*Config, error) {
	for _, r := range renamed {
		if get(r.old) != "" {
			return nil, fmt.Errorf("%s was removed: %s", r.old, r.hint)
		}
	}
	mode, err := ParseMode(get("ENCODE_MODE"))
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
		ControlURL:         strings.TrimSuffix(get("CONTROL_URL"), "/"),
		TLSCert:            get("TLS_CERT"),
		TLSKey:             get("TLS_KEY"),
		JellyfinURL:        strings.TrimSuffix(get("JELLYFIN_URL"), "/"),
		JellyfinToken:      get("JELLYFIN_TOKEN"),
		JellyfinLibraryDir: get("JELLYFIN_LIBRARY_DIR"),
		WebhookAddr:        or(get("WEBHOOK_ADDR"), "127.0.0.1:8201"),
		WebhookToken:       get("WEBHOOK_TOKEN"),
		Output:             encode.DefaultProfile(),
		AdminAddr:          get("ADMIN_ADDR"),
		AdminURL:           strings.TrimSuffix(get("ADMIN_URL"), "/"),
		AdminTLSCert:       get("ADMIN_TLS_CERT"),
		AdminTLSKey:        get("ADMIN_TLS_KEY"),
		MetricsAddr:        get("METRICS_ADDR"),
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
	if c.AdminAddr != "" && (c.JellyfinURL == "") {
		return errors.New("ADMIN_ADDR needs JELLYFIN_URL, because login goes through Jellyfin")
	}
	if (c.AdminTLSCert == "") != (c.AdminTLSKey == "") {
		return errors.New("ADMIN_TLS_CERT and ADMIN_TLS_KEY must be set together")
	}
	if c.AdminURL != "" {
		u, err := url.Parse(c.AdminURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("ADMIN_URL must be a full http or https address, such as https://example.com/jellyfinner-admin")
		}
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
	for _, u := range []struct{ key, value string }{{"CONTROL_URL", c.ControlURL}, {"ADMIN_URL", c.AdminURL}, {"JELLYFIN_URL", c.JellyfinURL}} {
		if _, err := URLBasePath(u.value); err != nil {
			return fmt.Errorf("%s: %w", u.key, err)
		}
	}
	return nil
}

var basePathPattern = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+$`)

// BasePath reads a path prefix, such as "/jellyfinner". It always starts with a
// slash and never ends with one, and "" means none.
func BasePath(s string) (string, error) {
	s = strings.TrimSuffix(s, "/")
	if s == "" {
		return "", nil
	}
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	if !basePathPattern.MatchString(s) {
		return "", fmt.Errorf("path %q may only use letters, digits and . _ ~ - between slashes", s)
	}
	for _, seg := range strings.Split(s[1:], "/") {
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("path %q must not contain . or ..", s)
		}
	}
	return s, nil
}

// URLBasePath is the path prefix of a URL, "" when it has none.
func URLBasePath(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	return BasePath(u.Path)
}

// ControlBasePath is the prefix of CONTROL_URL, which validate has checked.
func (c *Config) ControlBasePath() string {
	p, _ := URLBasePath(c.ControlURL)
	return p
}

// AdminBasePath is the prefix of ADMIN_URL, which validate has checked.
func (c *Config) AdminBasePath() string {
	p, _ := URLBasePath(c.AdminURL)
	return p
}

// AdminHTTPS reports whether people reach the admin UI over https, so its
// session cookie must never travel in the clear.
func (c *Config) AdminHTTPS() bool {
	return strings.HasPrefix(strings.ToLower(c.AdminURL), "https://")
}

// TrimPrefix removes a path prefix when the path is under it, and otherwise
// returns the path unchanged. That way a listener works whether or not a
// reverse proxy strips the prefix. It matches only at a slash, so /jfx is
// not under /jf.
func TrimPrefix(path, prefix string) string {
	if prefix == "" {
		return path
	}
	if path == prefix {
		return "/"
	}
	if rest, ok := strings.CutPrefix(path, prefix); ok && strings.HasPrefix(rest, "/") {
		return rest
	}
	return path
}

// StripPrefix serves next with the prefix removed, as TrimPrefix does.
func StripPrefix(prefix string, next http.Handler) http.Handler {
	if prefix == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trimmed := TrimPrefix(r.URL.Path, prefix)
		if trimmed == r.URL.Path {
			next.ServeHTTP(w, r)
			return
		}
		r2 := new(http.Request)
		*r2 = *r
		u := *r.URL
		u.Path = trimmed
		u.RawPath = ""
		if r.URL.RawPath != "" {
			u.RawPath = TrimPrefix(r.URL.RawPath, prefix)
		}
		r2.URL = &u
		next.ServeHTTP(w, r2)
	})
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

// CheckControlURL enforces the transport rule for nodes: HTTPS anywhere, plain
// HTTP only inside a private network. That means a private address, which in
// practice is a WireGuard peer, or a container network name such as
// orchestrator or host.docker.internal.
func CheckControlURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("control URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		addr, err := netip.ParseAddr(host)
		if err != nil {
			if privateName(host) {
				return nil
			}
			return fmt.Errorf("control URL %q: plain http needs a private address or a container network name", raw)
		}
		if !addr.IsPrivate() && !addr.IsLoopback() {
			return fmt.Errorf("control URL %q: plain http is only allowed to a private address", raw)
		}
		return nil
	}
	return fmt.Errorf("control URL %q: scheme must be https or http", raw)
}

// privateName reports whether a hostname cannot be a public one: a single label
// like a Compose service, or anything under the reserved .internal domain.
func privateName(host string) bool {
	return host != "" && (!strings.Contains(host, ".") || strings.HasSuffix(host, ".internal"))
}

// CheckListenAddr is the same rule for a plain HTTP listener, which must bind a
// private address. In a container every interface is private to it, and the
// published port decides the real exposure, so the image may bind them all.
func CheckListenAddr(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%q: plain http needs a private IP address, not a hostname", listen)
	}
	if addr.IsUnspecified() && os.Getenv("JELLYFINNER_IN_CONTAINER") == "1" {
		return nil
	}
	if !addr.IsPrivate() && !addr.IsLoopback() {
		return fmt.Errorf("%q: plain http is only allowed on a private address", listen)
	}
	return nil
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
