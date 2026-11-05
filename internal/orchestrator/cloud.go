package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"jellyfinner/internal/cloud"
	"jellyfinner/internal/config"
	"jellyfinner/internal/model"
)

const (
	// cloudInterval is how often the scaler looks at the queue and servers.
	cloudInterval = 30 * time.Second
	// cloudCooldown sets aside a type and location that could not be had.
	cloudCooldown = 15 * time.Minute
	// cloudMargin is how close to the end of a paid hour an idle server is
	// drained, leaving time to delete it before the next hour starts.
	cloudMargin = 5 * time.Minute
	// reconcileEvery is how often each provider's own list is compared with ours.
	reconcileEvery = 10 * time.Minute
	// orphanAge protects a server another process has just created from being
	// taken for an orphan before its row is written.
	orphanAge = 5 * time.Minute
	// priceTTL is how long quoted prices are used before asking again.
	priceTTL = 6 * time.Hour
	// cloudNodePrefix starts every rented server's node name.
	cloudNodePrefix = "cloud-"
	// metaPricesPrefix is followed by a provider name.
	metaPricesPrefix = "cloud_prices:"
)

// BuildProviders makes the providers the operator listed, in their order.
func BuildProviders(c config.Cloud) ([]cloud.Provider, error) {
	var out []cloud.Provider
	for _, p := range c.Providers {
		prov, err := cloud.New(p.Name, cloud.Settings{
			Token: p.Token, Project: p.Project, Types: p.Types, Locations: p.Locations, Image: p.Image, SSHKey: p.SSHKey,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, prov)
	}
	return out, nil
}

// CheckCloudProviders refuses a start that would leave servers running with
// nobody tracking them: every provider with a live server must still be listed.
func CheckCloudProviders(s *Service) error {
	rows, err := s.Store.CloudServers(s.Now())
	if err != nil {
		return err
	}
	listed := map[string]bool{}
	for _, p := range s.Cfg.Cloud.Providers {
		listed[p.Name] = true
	}
	for _, r := range rows {
		if r.Live() && !listed[r.Provider] {
			return fmt.Errorf("%s still has server %s (%s) tracked, but CLOUD_PROVIDERS no longer lists it: "+
				"put it back and run orchestrator cloud down --wait first, so the server is deleted", r.Provider, r.Name, r.ID)
		}
	}
	return nil
}

// Cloud rents workers when the queue backs up and deletes them when idle. Its
// state lives in the database, so serve and the CLI can both act on it.
type Cloud struct {
	svc       *Service
	cfg       config.Cloud
	providers []cloud.Provider
	seen      func() map[string]time.Time

	mu            sync.Mutex
	cluster       string
	cooldown      map[string]time.Time
	disabled      map[string]string
	notified      map[string]bool
	lastReconcile time.Time
	lastDelete    map[string]time.Time
	orphanNodes   map[string]bool
	prices        map[string]priceQuote
}

type priceQuote struct {
	At     time.Time     `json:"at"`
	Prices []cloud.Price `json:"prices"`
}

// NewCloud returns the scaler. seen reports when each node last called the
// control API; the CLI, which has no API, passes nil.
func NewCloud(svc *Service, providers []cloud.Provider, seen func() map[string]time.Time) (*Cloud, error) {
	fresh := make([]byte, 6)
	if _, err := rand.Read(fresh); err != nil {
		return nil, err
	}
	cluster, err := svc.Store.EnsureMeta(model.MetaCloudCluster, hex.EncodeToString(fresh))
	if err != nil {
		return nil, err
	}
	if seen == nil {
		seen = func() map[string]time.Time { return nil }
	}
	return &Cloud{
		svc: svc, cfg: svc.Cfg.Cloud, providers: providers, seen: seen, cluster: cluster,
		cooldown: map[string]time.Time{}, disabled: map[string]string{}, notified: map[string]bool{},
		lastDelete: map[string]time.Time{}, orphanNodes: map[string]bool{}, prices: map[string]priceQuote{},
	}, nil
}

// HoursCap is CLOUD_MAX_HOURS_PER_DAY.
func (c *Cloud) HoursCap() float64 { return c.cfg.MaxHoursPerDay }

// Providers returns the providers in the operator's order.
func (c *Cloud) Providers() []cloud.Provider { return c.providers }

// Cluster is the label value that marks this orchestrator's servers.
func (c *Cloud) Cluster() string { return c.cluster }

func (c *Cloud) provider(name string) cloud.Provider {
	for _, p := range c.providers {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

func (c *Cloud) now() time.Time { return c.svc.Now() }

// Paused reports whether new servers are held back.
func (c *Cloud) Paused() (bool, error) {
	v, err := c.svc.Store.Meta(model.MetaCloudPaused)
	return v == "1", err
}

// SetPaused stops or allows new servers. Running ones carry on and are
// deleted as they go idle.
func (c *Cloud) SetPaused(paused bool) error {
	v := ""
	if paused {
		v = "1"
	}
	return c.svc.Store.SetMeta(model.MetaCloudPaused, v)
}

// Run ticks until ctx ends.
func (c *Cloud) Run(ctx context.Context) {
	c.Tick(ctx)
	t := time.NewTicker(cloudInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Tick(ctx)
		}
	}
}

// Tick moves every server along, reconciles now and then, and starts a server
// if the queue needs one.
func (c *Cloud) Tick(ctx context.Context) {
	if err := c.Advance(ctx); err != nil {
		c.svc.Log.Printf("cloud: %v", err)
	}
	c.mu.Lock()
	due := c.now().Sub(c.lastReconcile) >= reconcileEvery
	if due {
		c.lastReconcile = c.now()
	}
	c.mu.Unlock()
	if due {
		if err := c.Reconcile(ctx); err != nil {
			c.svc.Log.Printf("cloud reconcile: %v", err)
		}
	}
	if err := c.scaleUp(ctx); err != nil {
		c.svc.Log.Printf("cloud: %v", err)
	}
}

func (c *Cloud) live() ([]*model.CloudServer, error) {
	rows, err := c.svc.Store.CloudServers(c.now().Add(time.Hour))
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, r := range rows {
		if r.Live() {
			out = append(out, r)
		}
	}
	return out, nil
}

// Advance moves each live server one step through its lifecycle: booting to
// ready, idle to draining near the end of a paid hour, draining to deleting
// once its job is done, and deleting to deleted once the provider confirms.
func (c *Cloud) Advance(ctx context.Context) error {
	rows, err := c.live()
	if err != nil {
		return err
	}
	running, err := c.svc.Store.RunningByNode()
	if err != nil {
		return err
	}
	nodes, err := c.svc.Store.LoadNodes()
	if err != nil {
		return err
	}
	paused := map[string]bool{}
	for _, n := range nodes {
		paused[n.Name] = n.Paused
	}
	seen := c.seen()
	now := c.now()
	var errs []error
	for _, r := range rows {
		before := *r
		if err := c.step(ctx, r, running[r.Name] > 0, paused[r.Name], seen[r.Name], now); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Name, err))
		}
		if *r != before {
			if err := c.svc.Store.UpdateCloudServer(r); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (c *Cloud) step(ctx context.Context, r *model.CloudServer, busy, paused bool, seen, now time.Time) error {
	p := c.provider(r.Provider)
	created := time.Unix(r.Created, 0)
	if r.State == model.CloudBooting {
		switch {
		case !seen.IsZero() && !seen.Before(created):
			r.State, r.FirstSeen = model.CloudReady, seen.Unix()
		case now.Sub(created) >= c.cfg.BootTimeout:
			c.coolDown(r.Provider, r.Type, r.Location, now)
			c.svc.Log.Printf("cloud: %s did not call in within %s, deleting it", r.Name, c.cfg.BootTimeout)
			r.State = model.CloudDraining
		}
	}
	if r.State == model.CloudReady {
		if busy {
			r.IdleSince = 0
		} else if r.IdleSince == 0 {
			r.IdleSince = now.Unix()
		}
		step := time.Hour
		if p != nil {
			step = p.BillingStep()
		}
		idleFor := time.Duration(0)
		if r.IdleSince != 0 {
			idleFor = now.Sub(time.Unix(r.IdleSince, 0))
		}
		endOfHour := cloud.PaidUntil(created, now, step).Sub(now) <= cloudMargin
		switch {
		case paused:
			r.State = model.CloudDraining
		case now.Sub(created) >= c.cfg.MaxLifetime:
			c.svc.Log.Printf("cloud: %s reached CLOUD_MAX_LIFETIME, draining it", r.Name)
			r.State = model.CloudDraining
		case !busy && idleFor >= c.cfg.Idle && endOfHour:
			r.State = model.CloudDraining
		}
	}
	if r.State == model.CloudDraining {
		if !paused {
			if err := c.svc.Nodes.SetPaused(r.Name, true); err != nil && !errors.Is(err, model.ErrNotFound) {
				return err
			}
		}
		if busy {
			return nil
		}
		r.State, r.DeleteAt = model.CloudDeleting, now.Unix()
	}
	if r.State == model.CloudDeleting {
		return c.confirmDelete(ctx, p, r, now)
	}
	return nil
}

// confirmDelete sends the delete, again with backoff while unconfirmed, and
// only records the server deleted once the provider says it is gone.
func (c *Cloud) confirmDelete(ctx context.Context, p cloud.Provider, r *model.CloudServer, now time.Time) error {
	if p == nil {
		return fmt.Errorf("provider %s is not listed, so the delete cannot be confirmed", r.Provider)
	}
	srv := cloud.Server{ID: r.ID, Name: r.Name, Resources: r.Resources}
	key := r.Provider + "/" + r.ID
	c.mu.Lock()
	last, tried := c.lastDelete[key]
	c.mu.Unlock()
	if tried {
		gone, err := p.Gone(ctx, srv)
		if err != nil {
			return c.overdue(ctx, r, now, err)
		}
		if gone {
			r.State, r.Deleted = model.CloudDeleted, now.Unix()
			c.mu.Lock()
			delete(c.lastDelete, key)
			c.mu.Unlock()
			if err := c.svc.Nodes.Remove(r.Name); err != nil && !errors.Is(err, model.ErrNotFound) {
				return err
			}
			c.svc.Log.Printf("cloud: %s on %s is deleted, confirmed by the provider", r.Name, r.Provider)
			return nil
		}
		// Back off: 30 s, then doubling, up to 5 minutes between deletes.
		wait := min(5*time.Minute, cloudInterval<<min(6, int(now.Sub(time.Unix(r.DeleteAt, 0))/(5*time.Minute))))
		if now.Sub(last) < wait {
			return c.overdue(ctx, r, now, nil)
		}
	}
	c.mu.Lock()
	c.lastDelete[key] = now
	c.mu.Unlock()
	err := p.Delete(ctx, srv, c.cluster)
	if errors.Is(err, cloud.ErrForeign) {
		// Never delete what is not ours; stop tracking it instead.
		r.State, r.Deleted = model.CloudDeleted, now.Unix()
		c.svc.send(ctx, fmt.Sprintf("cloud: %s server %s does not carry this orchestrator's tag, so it was not deleted and is no longer tracked", r.Provider, r.ID))
		return nil
	}
	return c.overdue(ctx, r, now, err)
}

// overdue alerts once if a delete stays unconfirmed past CLOUD_DELETE_TIMEOUT.
func (c *Cloud) overdue(ctx context.Context, r *model.CloudServer, now time.Time, err error) error {
	if !r.Alerted && now.Sub(time.Unix(r.DeleteAt, 0)) >= c.cfg.DeleteTimeout {
		r.Alerted = true
		c.svc.send(ctx, fmt.Sprintf("cloud: deleting %s (%s server %s) is not confirmed after %s and it may still be billed. "+
			"Jellyfinner keeps retrying; check the %s console.", r.Name, r.Provider, r.ID, c.cfg.DeleteTimeout, r.Provider))
	}
	return err
}

// DeleteOverdue reports whether a server's delete is unconfirmed past the timeout.
func (c *Cloud) DeleteOverdue(r *model.CloudServer) bool {
	return r.State == model.CloudDeleting && c.now().Sub(time.Unix(r.DeleteAt, 0)) >= c.cfg.DeleteTimeout
}

func (c *Cloud) coolDown(provider, typ, location string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cooldown[provider+"/"+typ+"/"+location] = now.Add(cloudCooldown)
}

func (c *Cloud) coolingDown(provider, typ, location string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return now.Before(c.cooldown[provider+"/"+typ+"/"+location])
}

// Disabled returns the providers set aside for configuration errors, with why.
func (c *Cloud) Disabled() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.disabled))
	for k, v := range c.disabled {
		out[k] = v
	}
	return out
}

// notifyOnce sends a message the first time its key is seen.
func (c *Cloud) notifyOnce(ctx context.Context, key, msg string) {
	c.mu.Lock()
	sent := c.notified[key]
	c.notified[key] = true
	c.mu.Unlock()
	if !sent {
		c.svc.send(ctx, msg)
	} else {
		c.svc.Log.Print(msg)
	}
}

// Reconcile compares each provider's own list with ours. Servers carrying our
// tag that we do not track are deleted, servers we track that are gone are
// recorded so, and cloud nodes with no server are removed.
func (c *Cloud) Reconcile(ctx context.Context) error {
	rows, err := c.svc.Store.CloudServers(time.Unix(0, 0))
	if err != nil {
		return err
	}
	byID := map[string]*model.CloudServer{}
	names := map[string]bool{}
	for _, r := range rows {
		byID[r.Provider+"/"+r.ID] = r
		names[r.Name] = true
	}
	now := c.now()
	var errs []error
	for _, p := range c.providers {
		if _, off := c.Disabled()[p.Name()]; off {
			continue
		}
		listed, err := p.List(ctx, c.cluster)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Name(), err))
			continue
		}
		present := map[string]bool{}
		for _, s := range listed {
			present[s.ID] = true
			r := byID[p.Name()+"/"+s.ID]
			switch {
			case r == nil && !s.Created.IsZero() && now.Sub(s.Created) < orphanAge:
				// Possibly created a moment ago by another process.
			case r == nil:
				c.svc.Log.Printf("cloud: %s server %s carries our tag but is not tracked, deleting it", p.Name(), s.ID)
				orphan := &model.CloudServer{Provider: p.Name(), ID: s.ID, Name: s.Name, Type: s.Type, Location: s.Location,
					Resources: s.Resources, State: model.CloudDeleting, Created: s.Created.Unix(), DeleteAt: now.Unix()}
				if s.Created.IsZero() {
					orphan.Created = now.Unix()
				}
				if err := c.svc.Store.AddCloudServer(orphan); err != nil {
					errs = append(errs, err)
				}
			case !r.Live():
				// Recorded as deleted, yet still listed: it is not gone.
				c.svc.Log.Printf("cloud: %s was recorded deleted but %s still lists it, deleting again", r.Name, p.Name())
				r.State, r.Deleted, r.DeleteAt, r.Alerted = model.CloudDeleting, 0, now.Unix(), false
				if err := c.svc.Store.UpdateCloudServer(r); err != nil {
					errs = append(errs, err)
				}
			}
		}
		for _, r := range rows {
			if r.Provider != p.Name() || !r.Live() || r.State == model.CloudDeleting || present[r.ID] {
				continue
			}
			if now.Sub(time.Unix(r.Created, 0)) < orphanAge {
				continue
			}
			gone, err := p.Gone(ctx, cloud.Server{ID: r.ID, Resources: r.Resources})
			if err != nil || !gone {
				continue
			}
			c.svc.Log.Printf("cloud: %s is gone from %s without us deleting it", r.Name, p.Name())
			r.State, r.Deleted = model.CloudDeleted, now.Unix()
			if err := c.svc.Store.UpdateCloudServer(r); err != nil {
				errs = append(errs, err)
			}
			if err := c.svc.Nodes.Remove(r.Name); err != nil && !errors.Is(err, model.ErrNotFound) {
				errs = append(errs, err)
			}
		}
	}
	// A cloud node with no server row twice in a row is left over from a
	// create that failed half-way, and its token is worth nothing to keep.
	nodes, err := c.svc.Store.LoadNodes()
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	c.mu.Lock()
	candidates := c.orphanNodes
	c.orphanNodes = map[string]bool{}
	c.mu.Unlock()
	for _, n := range nodes {
		if n.Edge() || !strings.HasPrefix(n.Name, cloudNodePrefix) || names[n.Name] {
			continue
		}
		if candidates[n.Name] {
			if err := c.svc.Nodes.Remove(n.Name); err != nil && !errors.Is(err, model.ErrNotFound) {
				errs = append(errs, err)
			}
			continue
		}
		c.mu.Lock()
		c.orphanNodes[n.Name] = true
		c.mu.Unlock()
	}
	return errors.Join(errs...)
}

// scaleUp starts one server if the queue has waited long enough or grown big
// enough, within CLOUD_MAX and the daily caps, unless paused.
func (c *Cloud) scaleUp(ctx context.Context) error {
	if paused, err := c.Paused(); err != nil || paused {
		return err
	}
	w, err := c.svc.Store.Waiting(c.cfg.Backfill)
	if err != nil || w.Count == 0 {
		return err
	}
	now := c.now()
	if now.Sub(time.Unix(w.Oldest, 0)) < c.cfg.Wait && w.Count < c.cfg.QueueThreshold {
		return nil
	}
	rows, err := c.live()
	if err != nil {
		return err
	}
	running, err := c.svc.Store.RunningByNode()
	if err != nil {
		return err
	}
	active, spare := 0, 0
	for _, r := range rows {
		switch r.State {
		case model.CloudBooting:
			active++
			spare++
		case model.CloudReady:
			active++
			if running[r.Name] == 0 {
				spare++
			}
		case model.CloudDraining, model.CloudDeleting:
			active++ // still billed, and still counts against CLOUD_MAX
		}
	}
	if w.Count <= spare || active >= c.cfg.Max {
		return nil
	}
	_, err = c.Start(ctx, "")
	return err
}

// ErrCapped means a daily limit stops a new server.
var ErrCapped = errors.New("daily cloud limit reached")

// ErrNoProvider means no listed provider could create a server.
var ErrNoProvider = errors.New("no cloud provider could start a server")

// Start creates one server, trying providers in the operator's order, then
// each one's types and locations in order. With only set, it tries that
// provider alone. It skips the queue triggers, but never the limits.
func (c *Cloud) Start(ctx context.Context, only string) (*model.CloudServer, error) {
	if len(c.providers) == 0 {
		return nil, errors.New("no cloud providers are listed in CLOUD_PROVIDERS")
	}
	if only != "" && c.provider(only) == nil {
		return nil, fmt.Errorf("%s is not listed in CLOUD_PROVIDERS", only)
	}
	rows, err := c.live()
	if err != nil {
		return nil, err
	}
	if len(rows) >= c.cfg.Max {
		return nil, fmt.Errorf("%d servers are running, the CLOUD_MAX limit", len(rows))
	}
	now := c.now()
	day := now.UTC().Format("2006-01-02")
	usage, err := c.Usage(ctx)
	if err != nil {
		return nil, err
	}
	if usage.HoursToday+1 > c.cfg.MaxHoursPerDay {
		c.notifyOnce(ctx, "hours:"+day, fmt.Sprintf("cloud: %.0f of %.0f billed hours used today (CLOUD_MAX_HOURS_PER_DAY), so no new server starts until tomorrow (UTC)",
			usage.HoursToday, c.cfg.MaxHoursPerDay))
		return nil, ErrCapped
	}
	var tried []string
	for _, p := range c.providers {
		if only != "" && p.Name() != only {
			continue
		}
		if why, off := c.Disabled()[p.Name()]; off {
			tried = append(tried, p.Name()+": set aside, "+why)
			continue
		}
		for _, typ := range c.cfg.ProviderTypes(p.Name()) {
			for _, loc := range c.cfg.ProviderLocations(p.Name()) {
				if c.coolingDown(p.Name(), typ, loc, now) {
					continue
				}
				price, _ := c.price(ctx, p, typ, loc)
				if c.cfg.MaxSpendPerDay > 0 {
					if err := c.spendAllows(usage, price); err != nil {
						c.notifyOnce(ctx, "spend:"+day, "cloud: "+err.Error())
						return nil, err
					}
				}
				r, err := c.create(ctx, p, typ, loc, price)
				if err == nil {
					return r, nil
				}
				tried = append(tried, fmt.Sprintf("%s %s %s: %v", p.Name(), typ, loc, err))
				if errors.Is(err, cloud.ErrConfig) {
					c.mu.Lock()
					c.disabled[p.Name()] = err.Error()
					c.mu.Unlock()
					c.notifyOnce(ctx, "config:"+p.Name(), fmt.Sprintf("cloud: %s is set aside until restart, because its settings were refused: %v", p.Name(), err))
					break
				}
				c.coolDown(p.Name(), typ, loc, now)
			}
			if _, off := c.Disabled()[p.Name()]; off {
				break
			}
		}
	}
	if len(tried) == 0 {
		return nil, fmt.Errorf("%w: every option is cooling down after a recent failure", ErrNoProvider)
	}
	return nil, fmt.Errorf("%w:\n  %s", ErrNoProvider, strings.Join(tried, "\n  "))
}

// spendAllows checks CLOUD_MAX_SPEND_PER_DAY, which only works when every
// price shares one currency.
func (c *Cloud) spendAllows(u Usage, price cloud.Price) error {
	if price.Currency == "" {
		return errors.New("CLOUD_MAX_SPEND_PER_DAY is set but this option has no price, so its cost cannot be checked")
	}
	for cur := range u.SpendToday {
		if cur != "" && cur != price.Currency {
			return fmt.Errorf("CLOUD_MAX_SPEND_PER_DAY needs every provider to price in one currency, but %s and %s are both used", cur, price.Currency)
		}
	}
	if u.SpendToday[price.Currency]+price.Hourly > c.cfg.MaxSpendPerDay {
		return fmt.Errorf("about %.2f %s spent today; another hour would pass CLOUD_MAX_SPEND_PER_DAY of %.2f", u.SpendToday[price.Currency], price.Currency, c.cfg.MaxSpendPerDay)
	}
	return nil
}

func (c *Cloud) create(ctx context.Context, p cloud.Provider, typ, loc string, price cloud.Price) (*model.CloudServer, error) {
	suffix := make([]byte, 3)
	if _, err := rand.Read(suffix); err != nil {
		return nil, err
	}
	name := cloudNodePrefix + p.Name() + "-" + hex.EncodeToString(suffix)
	added, err := c.svc.AddNode(NodeSpec{Name: name})
	if err != nil {
		return nil, err
	}
	userData, err := cloud.UserData(cloud.BootValues{Image: c.cfg.WorkerImage, URL: c.svc.Cfg.ControlURL, Token: added.Token})
	if err != nil {
		_ = c.svc.Nodes.Remove(name)
		return nil, fmt.Errorf("%w: %w", cloud.ErrConfig, err)
	}
	srv, err := p.Create(ctx, cloud.Spec{Name: name, Type: typ, Location: loc, UserData: userData, Cluster: c.cluster})
	if err != nil {
		_ = c.svc.Nodes.Remove(name)
		return nil, err
	}
	created := srv.Created
	if created.IsZero() || created.After(c.now()) {
		created = c.now()
	}
	r := &model.CloudServer{Provider: p.Name(), ID: srv.ID, Name: name, Type: typ, Location: loc, Resources: srv.Resources,
		State: model.CloudBooting, Created: created.Unix(), HourlyPrice: price.Hourly, Currency: price.Currency}
	if err := c.svc.Store.AddCloudServer(r); err != nil {
		// Reconcile finds it by its tag and deletes it.
		return nil, fmt.Errorf("server %s created but not recorded: %w", srv.ID, err)
	}
	c.svc.send(ctx, fmt.Sprintf("cloud: started %s, %s %s in %s", name, p.Name(), typ, loc))
	return r, nil
}

// Drain pauses a server's node; it is deleted once its job ends.
func (c *Cloud) Drain(name string) error {
	rows, err := c.live()
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Name != name {
			continue
		}
		if err := c.svc.Nodes.SetPaused(name, true); err != nil && !errors.Is(err, model.ErrNotFound) {
			return err
		}
		if r.State == model.CloudBooting || r.State == model.CloudReady {
			r.State = model.CloudDraining
			return c.svc.Store.UpdateCloudServer(r)
		}
		return nil
	}
	return fmt.Errorf("cloud server %s: %w", name, model.ErrNotFound)
}

// DrainAll drains every live server and returns how many.
func (c *Cloud) DrainAll() (int, error) {
	rows, err := c.live()
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		if err := c.Drain(r.Name); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

// price quotes one option, from a cache kept six hours and in the database,
// so estimates still work while a provider's API is down.
func (c *Cloud) price(ctx context.Context, p cloud.Provider, typ, loc string) (cloud.Price, time.Time) {
	q := c.quotes(ctx, p)
	for _, pr := range q.Prices {
		if pr.Type == typ && pr.Location == loc {
			return pr, q.At
		}
	}
	return cloud.Price{Type: typ, Location: loc}, q.At
}

func (c *Cloud) quotes(ctx context.Context, p cloud.Provider) priceQuote {
	c.mu.Lock()
	q, ok := c.prices[p.Name()]
	c.mu.Unlock()
	if !ok {
		if raw, err := c.svc.Store.Meta(metaPricesPrefix + p.Name()); err == nil && raw != "" {
			_ = json.Unmarshal([]byte(raw), &q)
		}
	}
	if c.now().Sub(q.At) < priceTTL {
		c.mu.Lock()
		c.prices[p.Name()] = q
		c.mu.Unlock()
		return q
	}
	fresh, err := p.Prices(ctx)
	if err != nil {
		c.svc.Log.Printf("cloud: %s prices: %v", p.Name(), err)
		return q
	}
	q = priceQuote{At: c.now(), Prices: fresh}
	c.mu.Lock()
	c.prices[p.Name()] = q
	c.mu.Unlock()
	if raw, err := json.Marshal(q); err == nil {
		_ = c.svc.Store.SetMeta(metaPricesPrefix+p.Name(), string(raw))
	}
	return q
}
