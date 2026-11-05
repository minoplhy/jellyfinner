package orchestrator

import (
	"context"
	"math"
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

// Usage adds up billed hours and spending from the servers table. Prices are
// the ones quoted when each server started.
func (c *Cloud) Usage(context.Context) (Usage, error) {
	now := c.now()
	day, month := dayStart(now), monthStart(now)
	rows, err := c.svc.Store.CloudServers(month)
	if err != nil {
		return Usage{}, err
	}
	u := Usage{SpendToday: map[string]float64{}}
	index := map[string]int{}
	for _, r := range rows {
		step := c.billingStep(r.Provider)
		created, end := time.Unix(r.Created, 0), c.end(r)
		today := cloud.HoursStarted(created, end, step, day, day.Add(24*time.Hour))
		monthHours := cloud.HoursStarted(created, end, step, month, month.AddDate(0, 1, 0))
		u.HoursToday += today
		u.SpendToday[r.Currency] += today * r.HourlyPrice
		key := r.Provider + "/" + r.Currency
		i, ok := index[key]
		if !ok {
			i = len(u.Spend)
			index[key] = i
			u.Spend = append(u.Spend, ProviderSpend{Provider: r.Provider, Currency: r.Currency})
		}
		u.Spend[i].HoursToday += today
		u.Spend[i].Today += today * r.HourlyPrice
		u.Spend[i].Month += monthHours * r.HourlyPrice
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
}

// Estimate is the what-if table, plus the queue it prices.
type Estimate struct {
	Rows        []EstimateRow
	QueueTitles int
	QueueGB     float64
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
		m.jobs += work[r.Name].Jobs
		m.bytes += work[r.Name].SourceBytes
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
					if row.GBPerHour > 0 && est.QueueGB > 0 {
						row.QueueHours = est.QueueGB / row.GBPerHour
						row.QueueCost = math.Ceil(row.QueueHours) * price.Hourly
					}
				}
				est.Rows = append(est.Rows, row)
			}
		}
	}
	return est, nil
}
