// Package cloud rents worker servers by the hour from providers the operator
// has added. Every provider is optional and none is preferred: each adapter
// registers itself under its name, and the orchestrator only ever sees the
// Provider interface, in the order the operator listed them.
package cloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Labels or tags on every server. The cluster value keeps two orchestrators
// that share a cloud project from touching each other's servers.
const (
	LabelRole    = "jellyfinner"
	RoleWorker   = "worker"
	LabelCluster = "jellyfinner-cluster"
)

// Errors an adapter returns, so the scaler can decide what to do next.
var (
	// ErrUnavailable means this type and location cannot be had now: out of
	// stock, over quota, rate limited or an outage. Try the next option.
	ErrUnavailable = errors.New("unavailable")
	// ErrConfig means the settings are wrong, such as a bad token or an unknown
	// type. Retrying cannot help, so the provider is set aside.
	ErrConfig = errors.New("configuration")
	// ErrForeign means a server does not carry this cluster's tag, so it is
	// never deleted, whatever asked for it.
	ErrForeign = errors.New("server belongs to another cluster")
)

// Settings are one provider's values from CLOUD_<PROVIDER>_*.
type Settings struct {
	Token     string
	Project   string // only providers that group servers in projects
	Types     []string
	Locations []string
	Image     string
	SSHKey    string

	// BaseURL and HTTP replace the real API in tests.
	BaseURL string
	HTTP    *http.Client
}

// Spec is a server to create.
type Spec struct {
	Name     string
	Type     string
	Location string
	UserData string
	Cluster  string
}

// Server is a server as the provider reports it. Resources lists extras an
// adapter must also delete, encoded as it likes.
type Server struct {
	ID        string
	Name      string
	Type      string
	Location  string
	Resources string
	Cluster   string
	Created   time.Time
}

// Source is where a billing fact comes from, so operators can check it.
type Source struct {
	Title string
	URL   string
}

// Billing describes how a provider charges, with its sources.
type Billing struct {
	// Note is shown in the UI and docs as is.
	Note    string
	Sources []Source
	// Checked is the date the note was last compared with its sources.
	Checked string
}

// Price is the cost of one type in one location, per hour, excluding VAT
// unless Taxed. It includes extras every server needs, such as an address.
type Price struct {
	Type     string
	Location string
	Hourly   float64
	Currency string
	Taxed    bool
}

// Provider is one cloud.
type Provider interface {
	// Name is the registry name, also used in settings and node names.
	Name() string
	// BillingStep is the unit billing rounds up to.
	BillingStep() time.Duration
	// Billing explains the rules behind BillingStep, with sources.
	Billing() Billing
	// Prices quotes every configured type and location.
	Prices(ctx context.Context) ([]Price, error)
	// Create starts a server, labelled with spec.Cluster.
	Create(ctx context.Context, spec Spec) (Server, error)
	// Delete removes a server and its extras. A server that is already gone is
	// a success. One without this cluster's label is refused with ErrForeign.
	Delete(ctx context.Context, s Server, cluster string) error
	// Gone reports whether a server and its extras no longer exist, which is
	// when billing stops.
	Gone(ctx context.Context, s Server) (bool, error)
	// List returns every server labelled with this cluster.
	List(ctx context.Context, cluster string) ([]Server, error)
	// Check validates the token and every configured type, location and image.
	Check(ctx context.Context) error
}

// Factory builds a provider from its settings, or explains what is missing.
type Factory func(Settings) (Provider, error)

var (
	mu        sync.Mutex
	factories = map[string]Factory{}
)

// Register makes an adapter available under a name. Adapters call it from init.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[name]; dup {
		panic("cloud: Register called twice for " + name)
	}
	factories[name] = f
}

// Names lists every registered adapter, alphabetically, so no listing ever
// suggests an order.
func Names() []string {
	mu.Lock()
	defer mu.Unlock()
	names := make([]string, 0, len(factories))
	for name := range factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// New builds the named provider.
func New(name string, s Settings) (Provider, error) {
	mu.Lock()
	f, ok := factories[name]
	mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown cloud provider %q, known ones are %v", name, Names())
	}
	if s.Token == "" {
		return nil, fmt.Errorf("%s: CLOUD_%s_TOKEN is required", name, envName(name))
	}
	if len(s.Types) == 0 || len(s.Locations) == 0 {
		return nil, fmt.Errorf("%s: CLOUD_%s_TYPES and CLOUD_%s_LOCATIONS are required", name, envName(name), envName(name))
	}
	if s.HTTP == nil {
		s.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	return f(s)
}

func envName(name string) string {
	out := []byte(name)
	for i, c := range out {
		if c >= 'a' && c <= 'z' {
			out[i] = c - 'a' + 'A'
		}
	}
	return string(out)
}

// EnvName is the settings prefix for a provider, such as CLOUD_EXAMPLE.
func EnvName(name string) string { return "CLOUD_" + envName(name) }
