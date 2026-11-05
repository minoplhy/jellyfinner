package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"jellyfinner/internal/model"
	"jellyfinner/internal/pipeline"
)

// speedWindow is how far back the backlog forecast measures speed.
const speedWindow = 30 * 24 * time.Hour

// projectAfter is how much of a month must pass before it is projected.
const projectAfter = 48 * time.Hour

// WorkRow is what one worker did in a period. Rented servers are grouped by
// provider and type, because each lives only an hour or two.
type WorkRow struct {
	Name     string // worker name, or "hetzner ccx13"
	Cloud    bool
	Provider string
	Type     string

	Published int
	Copies    int // video stream-copied, not encoded
	Failed    int
	Cancelled int
	Retries   int // extra attempts the published titles needed

	SourceBytes int64 // pulled by the worker, for published titles
	OutputBytes int64 // pushed back

	TimedJobs  int
	TimedBytes int64
	TimedHours float64
}

// Encodes is how many published titles had their video encoded.
func (r WorkRow) Encodes() int { return r.Published - r.Copies }

// GBPerHour is source GB per hour of job time, over timed jobs only. Job time
// runs from claim to hand-back: pull, encode, verify and push.
func (r WorkRow) GBPerHour() float64 {
	if r.TimedHours <= 0 {
		return 0
	}
	return float64(r.TimedBytes) / 1e9 / r.TimedHours
}

// TypeSpend is a cloud type's spending in a month, with what it published.
type TypeSpend struct {
	CloudSpend
	Titles int
}

// CostPerTitle is zero when the type published nothing.
func (t TypeSpend) CostPerTitle() float64 {
	if t.Titles == 0 {
		return 0
	}
	return t.Cost / float64(t.Titles)
}

// Money is an amount in one currency. Currencies are never converted.
type Money struct {
	Currency string
	Amount   float64
}

// BacklogForecast is everything left, and how long it takes.
type BacklogForecast struct {
	Titles int
	Bytes  int64

	// Own workers together, at their speed over the last 30 days.
	OwnWorkers   int
	OwnJobs      int
	OwnGBPerHour float64
	OwnHours     float64

	// BudgetDays is how many days BACKFILL_DAILY_GB alone allows the backfill,
	// or 0 with no budget. The backlog cannot finish sooner.
	BackfillBytes int64
	BudgetDays    float64

	Cloud    []EstimateRow // nil when no provider is listed
	PricedAt time.Time
}

// OwnDays is the longer of the speed and budget limits, in days.
func (b BacklogForecast) OwnDays() float64 {
	days := b.OwnHours / 24
	if b.BudgetDays > days {
		return b.BudgetDays
	}
	return days
}

// BudgetBound reports whether the daily budget, not speed, sets OwnDays.
func (b BacklogForecast) BudgetBound() bool { return b.BudgetDays > b.OwnHours/24 }

// Summary is one UTC month: work done, traffic, cloud spending, and the
// backlog as it stands now.
type Summary struct {
	Month   time.Time // first instant of the month, UTC
	Current bool
	Prev    time.Time // zero when there is no earlier month with history
	Next    time.Time // zero for the current month

	Work  []WorkRow
	Total WorkRow

	CloudOn    bool
	CloudSpend []TypeSpend
	CloudTotal []Money

	// Projection scales the month so far to the whole month. It is set only
	// for the current month, once two days have passed, and once there is
	// something to scale.
	Projected        bool
	ProjectFactor    float64
	ProjectedCloud   []Money
	ProjectedTraffic int64

	Backlog BacklogForecast
}

// TrafficBytes is all bytes the workers moved for published titles.
func (s Summary) TrafficBytes() int64 { return s.Total.SourceBytes + s.Total.OutputBytes }

// ParseMonth reads YYYY-MM as a UTC month. Empty means the current month.
func ParseMonth(v string, now time.Time) (time.Time, error) {
	if v == "" {
		return monthStart(now), nil
	}
	t, err := time.Parse("2006-01", v)
	if err != nil {
		return time.Time{}, fmt.Errorf("month %q: use YYYY-MM, such as %s", v, now.UTC().Format("2006-01"))
	}
	if t.After(now) {
		return time.Time{}, fmt.Errorf("month %s has not started yet", v)
	}
	return t, nil
}

// Summary builds the usage summary for a month. c is nil when no cloud
// provider is listed, which leaves the cloud parts out.
func (s *Service) Summary(ctx context.Context, month time.Time, c *Cloud) (Summary, error) {
	now := s.Now()
	month = monthStart(month)
	end := month.AddDate(0, 1, 0)
	sum := Summary{Month: month, Current: !now.Before(month) && now.Before(end), CloudOn: c != nil}
	if !sum.Current {
		sum.Next = end
	}
	first, err := s.Store.FirstActivity()
	if err != nil {
		return sum, err
	}
	if first != 0 && monthStart(time.Unix(first, 0)).Before(month) {
		sum.Prev = month.AddDate(0, -1, 0)
	}

	cloudNodes, err := s.cloudNodeTypes()
	if err != nil {
		return sum, err
	}
	outcomes, err := s.Store.Outcomes(month, end)
	if err != nil {
		return sum, err
	}
	sum.Work, sum.Total = workRows(outcomes, cloudNodes)
	if c != nil {
		sum.Work = c.orderWork(sum.Work)
	}

	if c != nil {
		spend, err := c.Spend(month, end)
		if err != nil {
			return sum, err
		}
		titles := map[string]int{}
		for _, w := range sum.Work {
			if w.Cloud {
				titles[w.Provider+"/"+w.Type] += w.Published
			}
		}
		totals := map[string]float64{}
		var currencies []string
		for _, sp := range spend {
			sum.CloudSpend = append(sum.CloudSpend, TypeSpend{CloudSpend: sp, Titles: titles[sp.Provider+"/"+sp.Type]})
			if _, ok := totals[sp.Currency]; !ok {
				currencies = append(currencies, sp.Currency)
			}
			totals[sp.Currency] += sp.Cost
		}
		for _, cur := range currencies {
			sum.CloudTotal = append(sum.CloudTotal, Money{Currency: cur, Amount: totals[cur]})
		}
	}

	active := sum.TrafficBytes() > 0 || len(sum.CloudTotal) > 0
	if elapsed := now.Sub(month); sum.Current && active && elapsed >= projectAfter {
		sum.Projected = true
		sum.ProjectFactor = end.Sub(month).Hours() / elapsed.Hours()
		for _, m := range sum.CloudTotal {
			sum.ProjectedCloud = append(sum.ProjectedCloud, Money{Currency: m.Currency, Amount: m.Amount * sum.ProjectFactor})
		}
		sum.ProjectedTraffic = int64(float64(sum.TrafficBytes()) * sum.ProjectFactor)
	}

	sum.Backlog, err = s.backlogForecast(ctx, now, c, cloudNodes)
	return sum, err
}

// cloudNodeTypes maps every rented server's node name to provider and type.
func (s *Service) cloudNodeTypes() (map[string]model.CloudServer, error) {
	rows, err := s.Store.CloudServers(time.Time{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]model.CloudServer, len(rows))
	for _, r := range rows {
		out[r.Name] = *r
	}
	return out, nil
}

// workRows groups outcomes by worker, or by cloud type for rented servers.
// A malformed manifest counts as no output and a full encode, never an error.
func workRows(outcomes []model.JobOutcome, cloudNodes map[string]model.CloudServer) ([]WorkRow, WorkRow) {
	var rows []WorkRow
	index := map[string]int{}
	total := WorkRow{Name: "Total"}
	for _, o := range outcomes {
		key, row := o.Node, WorkRow{Name: o.Node}
		if srv, ok := cloudNodes[o.Node]; ok {
			key = "\x00" + srv.Provider + "/" + srv.Type
			row = WorkRow{Name: srv.Provider + " " + srv.Type, Cloud: true, Provider: srv.Provider, Type: srv.Type}
		} else if strings.HasPrefix(o.Node, cloudNodePrefix) {
			row.Cloud = true
		} else if o.Node == "" {
			key, row.Name = "", "never claimed"
		}
		i, ok := index[key]
		if !ok {
			i = len(rows)
			index[key] = i
			rows = append(rows, row)
		}
		for _, r := range []*WorkRow{&rows[i], &total} {
			addOutcome(r, o)
		}
	}
	sort.SliceStable(rows, func(a, b int) bool {
		if rows[a].Cloud != rows[b].Cloud {
			return !rows[a].Cloud
		}
		if rows[a].Cloud {
			return false // orderWork puts these in the operator's order
		}
		return rows[a].Name < rows[b].Name
	})
	return rows, total
}

func addOutcome(r *WorkRow, o model.JobOutcome) {
	switch o.State {
	case model.StateFailed:
		r.Failed++
		return
	case model.StateCancelled:
		r.Cancelled++
		return
	}
	r.Published++
	if o.Attempts > 1 {
		r.Retries += o.Attempts - 1
	}
	r.SourceBytes += o.SourceSize
	var m pipeline.Manifest
	if json.Unmarshal([]byte(o.Manifest), &m) == nil {
		if m.VideoCopy {
			r.Copies++
		}
		for _, f := range m.Files {
			r.OutputBytes += f.Size
		}
	}
	if o.Timed() {
		r.TimedJobs++
		r.TimedBytes += o.SourceSize
		r.TimedHours += float64(o.Finished-o.Started) / 3600
	}
}

// orderWork puts cloud rows in the operator's provider and type order, after
// the operator's own workers.
func (c *Cloud) orderWork(rows []WorkRow) []WorkRow {
	sort.SliceStable(rows, func(a, b int) bool {
		if rows[a].Cloud != rows[b].Cloud {
			return !rows[a].Cloud
		}
		if !rows[a].Cloud {
			return false
		}
		pa, pb := c.providerRank(rows[a].Provider), c.providerRank(rows[b].Provider)
		if pa != pb {
			return pa < pb
		}
		return c.typeRank(rows[a].Provider, rows[a].Type) < c.typeRank(rows[b].Provider, rows[b].Type)
	})
	return rows
}

// backlogForecast prices everything left. Own workers run side by side, so
// their speeds add up; each is measured over the last 30 days.
func (s *Service) backlogForecast(ctx context.Context, now time.Time, c *Cloud, cloudNodes map[string]model.CloudServer) (BacklogForecast, error) {
	var b BacklogForecast
	titles, bytes, err := s.Backlog()
	if err != nil {
		return b, err
	}
	b.Titles, b.Bytes = titles, bytes

	recent, err := s.Store.Outcomes(now.Add(-speedWindow), now.Add(time.Second))
	if err != nil {
		return b, err
	}
	rows, _ := workRows(recent, cloudNodes)
	for _, r := range rows {
		if r.Cloud || r.GBPerHour() == 0 {
			continue
		}
		b.OwnWorkers++
		b.OwnJobs += r.TimedJobs
		b.OwnGBPerHour += r.GBPerHour()
	}
	if b.OwnGBPerHour > 0 {
		b.OwnHours = float64(b.Bytes) / 1e9 / b.OwnGBPerHour
	}

	_, st, err := s.backfillPlan()
	if err != nil {
		return b, err
	}
	b.BackfillBytes = st.RemainingBytes
	if budget := s.Cfg.BackfillDailyBytes; budget > 0 && st.RemainingBytes > 0 {
		b.BudgetDays = float64(st.RemainingBytes) / float64(budget)
	}

	if c != nil {
		est, err := c.Estimate(ctx)
		if err != nil {
			return b, err
		}
		b.Cloud = est.Rows
		for _, r := range est.Rows {
			if !r.PricedAt.IsZero() {
				b.PricedAt = r.PricedAt
				break
			}
		}
	}
	return b, nil
}
