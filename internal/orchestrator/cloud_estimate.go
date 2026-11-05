package orchestrator

import (
	"context"
	"math"
	"sort"
	"time"

	"jellyfinner/internal/cloud"
	"jellyfinner/internal/model"
)

// estimateWindow is how far back cost per title and throughput look.
const estimateWindow = 30 * 24 * time.Hour

// ProviderSpend is one provider's estimated spending in one currency.
type ProviderSpend struct {
	Provider   string
	Currency   string
	HoursToday float64
	Today      float64
	Month      float64
}

// Usage is billed hours and estimated spending, today and this month (UTC).
type Usage struct {
	HoursToday float64
	SpendToday map[string]float64 // by currency
	Spend      []ProviderSpend
}

func (c *Cloud) billingStep(name string) time.Duration {
	if p := c.provider(name); p != nil {
		return p.BillingStep()
	}
	return time.Hour
}

func dayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// end is when a server stopped billing, or now if it has not.
func (c *Cloud) end(r *model.CloudServer) time.Time {
	if r.State == model.CloudDeleted && r.Deleted != 0 {
		return time.Unix(r.Deleted, 0)
	}
	return c.now()
}

// CloudSpend is what one provider's servers of one type cost over a period,
// in one currency, at the prices quoted when each started.
type CloudSpend struct {
	Provider string
	Type     string
	Currency string
	Servers  int
	Hours    float64
	Cost     float64
}

// Spend adds up the billing steps that started in [from, to), for each
// provider, type and currency. Rows follow the operator's provider order, and
// providers no longer listed come last. Today, this month and any past month
// all come from here.
func (c *Cloud) Spend(from, to time.Time) ([]CloudSpend, error) {
	rows, err := c.svc.Store.CloudServers(from)
	if err != nil {
		return nil, err
	}
	var out []CloudSpend
	index := map[string]int{}
	for _, r := range rows {
		created := time.Unix(r.Created, 0)
		if !created.Before(to) {
			continue
		}
		hours := cloud.HoursStarted(created, c.end(r), c.billingStep(r.Provider), from, to)
		if hours == 0 {
			continue
		}
		key := r.Provider + "/" + r.Type + "/" + r.Currency
		i, ok := index[key]
		if !ok {
			i = len(out)
			index[key] = i
			out = append(out, CloudSpend{Provider: r.Provider, Type: r.Type, Currency: r.Currency})
		}
		out[i].Servers++
		out[i].Hours += hours
		out[i].Cost += hours * r.HourlyPrice
	}
	sort.SliceStable(out, func(a, b int) bool {
		pa, pb := c.providerRank(out[a].Provider), c.providerRank(out[b].Provider)
		if pa != pb {
			return pa < pb
		}
		return c.typeRank(out[a].Provider, out[a].Type) < c.typeRank(out[b].Provider, out[b].Type)
	})
	return out, nil
}

// providerRank is a provider's place in CLOUD_PROVIDERS; unlisted ones last.
func (c *Cloud) providerRank(name string) int {
	for i, p := range c.providers {
		if p.Name() == name {
			return i
		}
	}
	return len(c.providers)
}

// typeRank is a type's place in that provider's TYPES; unlisted ones last.
func (c *Cloud) typeRank(provider, typ string) int {
	types := c.cfg.ProviderTypes(provider)
	for i, t := range types {
		if t == typ {
			return i
		}
	}
	return len(types)
}

// Usage adds up billed hours and spending today and this month (UTC).
func (c *Cloud) Usage(context.Context) (Usage, error) {
	now := c.now()
	day, month := dayStart(now), monthStart(now)
	today, err := c.Spend(day, day.Add(24*time.Hour))
	if err != nil {
		return Usage{}, err
	}
	thisMonth, err := c.Spend(month, month.AddDate(0, 1, 0))
	if err != nil {
		return Usage{}, err
	}
	u := Usage{SpendToday: map[string]float64{}}
	index := map[string]int{}
	row := func(provider, currency string) *ProviderSpend {
		key := provider + "/" + currency
		i, ok := index[key]
		if !ok {
			i = len(u.Spend)
			index[key] = i
			u.Spend = append(u.Spend, ProviderSpend{Provider: provider, Currency: currency})
		}
		return &u.Spend[i]
	}
	for _, s := range thisMonth {
		row(s.Provider, s.Currency).Month += s.Cost
	}
	for _, s := range today {
		u.HoursToday += s.Hours
		u.SpendToday[s.Currency] += s.Cost
		r := row(s.Provider, s.Currency)
		r.HoursToday += s.Hours
		r.Today += s.Cost
	}
	return u, nil
}

// ServerView is a live server with its estimated cost so far.
type ServerView struct {
	model.CloudServer
	Cost      float64
	PaidUntil time.Time
	Overdue   bool
}

// Servers lists live servers, oldest first.
func (c *Cloud) Servers() ([]ServerView, error) {
	rows, err := c.live()
	if err != nil {
		return nil, err
	}
	out := make([]ServerView, 0, len(rows))
	for _, r := range rows {
		step := c.billingStep(r.Provider)
		created := time.Unix(r.Created, 0)
		out = append(out, ServerView{CloudServer: *r, Cost: cloud.Cost(created, c.now(), step, r.HourlyPrice),
			PaidUntil: cloud.PaidUntil(created, c.now(), step), Overdue: c.DeleteOverdue(r)})
	}
	return out, nil
}

// ProviderBilling is a provider's billing rules, for the UI and docs.
type ProviderBilling struct {
	Name    string
	Billing cloud.Billing
}

// Billing lists the rules of every listed provider, in the operator's order.
func (c *Cloud) Billing() []ProviderBilling {
	out := make([]ProviderBilling, 0, len(c.providers))
	for _, p := range c.providers {
		out = append(out, ProviderBilling{Name: p.Name(), Billing: p.Billing()})
	}
	return out
}

// EstimateRow is one type in one location. Measured figures are zero until
// servers of that type have finished work in the last 30 days.
type EstimateRow struct {
	Provider string
	Type     string
	Location string
	Hourly   float64
	Currency string
	Taxed    bool
	PricedAt time.Time

	Jobs         int
	GBPerHour    float64
	CostPerTitle float64
	QueueHours   float64
	QueueCost    float64
	BacklogHours float64
	BacklogCost  float64
}

// Estimate is the what-if table, plus the queue and backlog it prices. The
// queue is what the scaler would rent for now; the backlog is everything left,
// the whole library's backfill included.
type Estimate struct {
	Rows          []EstimateRow
	QueueTitles   int
	QueueGB       float64
	BacklogTitles int
	BacklogGB     float64
}

// Backlog is all work left: every queued job, plus backfill titles not yet
// queued. It matches what backfill status reports, plus the queue.
func (s *Service) Backlog() (titles int, bytes int64, err error) {
	w, err := s.Store.Waiting(true)
	if err != nil {
		return 0, 0, err
	}
	_, st, err := s.backfillPlan()
	if err != nil {
		return 0, 0, err
	}
	return w.Count + st.Remaining, w.SourceBytes + st.RemainingBytes, nil
}

// clearCost is how long, and what, gb of source takes at a measured speed.
// Whole hours, as both providers bill.
func clearCost(gb, gbPerHour, hourly float64) (hours, cost float64) {
	if gb <= 0 || gbPerHour <= 0 {
		return 0, 0
	}
	hours = gb / gbPerHour
	return hours, math.Ceil(hours) * hourly
}

// Estimate prices every configured option and, where real work has been
// measured, what clearing the queue would cost. It never reorders anything:
// rows follow the operator's provider order.
func (c *Cloud) Estimate(ctx context.Context) (Estimate, error) {
	var est Estimate
	w, err := c.svc.Store.Waiting(c.cfg.Backfill)
	if err != nil {
		return est, err
	}
	est.QueueTitles, est.QueueGB = w.Count, float64(w.SourceBytes)/1e9
	titles, bytes, err := c.svc.Backlog()
	if err != nil {
		return est, err
	}
	est.BacklogTitles, est.BacklogGB = titles, float64(bytes)/1e9

	since := c.now().Add(-estimateWindow)
	rows, err := c.svc.Store.CloudServers(since)
	if err != nil {
		return est, err
	}
	work, err := c.svc.Store.WorkByNode(since)
	if err != nil {
		return est, err
	}
	type measured struct {
		hours, cost float64
		jobs        int
		bytes       int64
	}
	byType := map[string]*measured{}
	for _, r := range rows {
		if r.Created < since.Unix() {
			continue
		}
		key := r.Provider + "/" + r.Type
		m := byType[key]
		if m == nil {
			m = &measured{}
			byType[key] = m
		}
		step := c.billingStep(r.Provider)
		created := time.Unix(r.Created, 0)
		hours := float64(cloud.BilledSteps(created, c.end(r), step)) * step.Hours()
		m.hours += hours
		m.cost += hours * r.HourlyPrice
		m.jobs += work[r.Node].Jobs
		m.bytes += work[r.Node].SourceBytes
	}
	for _, p := range c.providers {
		for _, typ := range c.cfg.ProviderTypes(p.Name()) {
			for _, loc := range c.cfg.ProviderLocations(p.Name()) {
				price, at := c.price(ctx, p, typ, loc)
				row := EstimateRow{Provider: p.Name(), Type: typ, Location: loc, Hourly: price.Hourly,
					Currency: price.Currency, Taxed: price.Taxed, PricedAt: at}
				if m := byType[p.Name()+"/"+typ]; m != nil && m.jobs > 0 && m.hours > 0 {
					row.Jobs = m.jobs
					row.GBPerHour = float64(m.bytes) / 1e9 / m.hours
					row.CostPerTitle = m.cost / float64(m.jobs)
					row.QueueHours, row.QueueCost = clearCost(est.QueueGB, row.GBPerHour, price.Hourly)
					row.BacklogHours, row.BacklogCost = clearCost(est.BacklogGB, row.GBPerHour, price.Hourly)
				}
				est.Rows = append(est.Rows, row)
			}
		}
	}
	return est, nil
}
