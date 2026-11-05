package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"text/tabwriter"

	"jellyfinner/internal/notify"
	"jellyfinner/internal/orchestrator"
)

// usageCommand prints one UTC month: work done, traffic and cloud spending,
// then the backlog as it stands now. It is the Usage page as text.
func usageCommand(envFile string, args []string) error {
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	monthFlag := fs.String("month", "", "YYYY-MM; the current month if empty")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, store, err := open(envFile)
	if err != nil {
		return err
	}
	defer store.Close()
	svc := orchestrator.New(cfg, store, notify.New(cfg.DiscordURL, cfg.NtfyURL), nil, log.New(os.Stderr, "", 0))
	month, err := orchestrator.ParseMonth(*monthFlag, svc.Now())
	if err != nil {
		return err
	}
	var c *orchestrator.Cloud
	if cfg.Cloud.Enabled() {
		providers, err := orchestrator.BuildProviders(cfg.Cloud)
		if err != nil {
			return err
		}
		if c, err = orchestrator.NewCloud(svc, providers, nil); err != nil {
			return err
		}
	}
	sum, err := svc.Summary(context.Background(), month, c)
	if err != nil {
		return err
	}
	printSummary(sum, c)
	return nil
}

func gb(n int64) string { return fmt.Sprintf("%.1f GB", float64(n)/1e9) }

func printSummary(s orchestrator.Summary, c *orchestrator.Cloud) {
	fmt.Printf("%s, UTC. %d titles published, %s of source, at least %s of worker traffic.\n",
		s.Month.Format("January 2006"), s.Total.Published, gb(s.Total.SourceBytes), gb(s.TrafficBytes()))
	if s.Projected {
		var cloud []string
		for _, m := range s.ProjectedCloud {
			cloud = append(cloud, money(m.Amount, m.Currency))
		}
		extra := ""
		if len(cloud) > 0 {
			extra = ", " + strings.Join(cloud, " and ") + " of cloud"
		}
		fmt.Printf("If the rest of the month is like so far: about %s of traffic%s.\n", gb(s.ProjectedTraffic), extra)
	}

	fmt.Println("\nWork done")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "WORKER\tPUBLISHED\tENCODED\tCOPIED\tRETRIES\tFAILED\tCANCELLED\tDOWNLOADED\tUPLOADED\tGB PER HOUR")
	rows := s.Work
	if len(rows) > 0 {
		rows = append(rows, s.Total)
	}
	for _, r := range rows {
		speed := "not timed"
		if r.TimedJobs > 0 {
			speed = fmt.Sprintf("%.2f (%d timed)", r.GBPerHour(), r.TimedJobs)
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\n", r.Name, r.Published, r.Encodes(), r.Copies, r.Retries,
			r.Failed, r.Cancelled, gb(r.SourceBytes), gb(r.OutputBytes), speed)
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "(no job finished this month)")
	}
	_ = w.Flush()
	fmt.Println("Traffic counts published titles only, so it is a lower bound. Speed runs from claim to hand-back.")

	if s.CloudOn {
		fmt.Println("\nCloud spending, estimated")
		w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "PROVIDER\tTYPE\tSERVERS\tBILLED HOURS\tCOST\tTITLES\tPER TITLE")
		for _, t := range s.CloudSpend {
			per := "-"
			if t.Titles > 0 {
				per = money(t.CostPerTitle(), t.Currency)
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%.1f\t%s\t%d\t%s\n", t.Provider, t.Type, t.Servers, t.Hours, money(t.Cost, t.Currency), t.Titles, per)
		}
		if len(s.CloudSpend) == 0 {
			fmt.Fprintln(w, "(no cloud server ran this month)")
		}
		_ = w.Flush()
		for _, m := range s.CloudTotal {
			fmt.Printf("Total: %s\n", money(m.Amount, m.Currency))
		}
	}

	b := s.Backlog
	fmt.Printf("\nBacklog now: %d titles, %s of source.\n", b.Titles, gb(b.Bytes))
	switch {
	case b.OwnGBPerHour > 0 && b.BudgetBound():
		fmt.Printf("  Your workers (%d, %d timed jobs, %.2f GB per hour): at least %.1f days, set by BACKFILL_DAILY_GB.\n",
			b.OwnWorkers, b.OwnJobs, b.OwnGBPerHour, b.OwnDays())
	case b.OwnGBPerHour > 0:
		fmt.Printf("  Your workers (%d, %d timed jobs, %.2f GB per hour): about %.1f days.\n",
			b.OwnWorkers, b.OwnJobs, b.OwnGBPerHour, b.OwnDays())
	case b.BudgetDays > 0:
		fmt.Printf("  Your workers: no timed jobs yet. BACKFILL_DAILY_GB alone needs at least %.1f days.\n", b.BudgetDays)
	default:
		fmt.Println("  Your workers: no timed jobs yet, so no forecast.")
	}
	for _, r := range b.Cloud {
		switch {
		case r.Jobs == 0:
			fmt.Printf("  %s %s in %s: no jobs yet.\n", r.Provider, r.Type, r.Location)
		case r.BacklogHours > 0:
			fmt.Printf("  %s %s in %s (%d jobs, %.2f GB per hour): about %.1f billed hours, %s.\n",
				r.Provider, r.Type, r.Location, r.Jobs, r.GBPerHour, r.BacklogHours, money(r.BacklogCost, r.Currency))
		default:
			fmt.Printf("  %s %s in %s (%d jobs, %.2f GB per hour).\n", r.Provider, r.Type, r.Location, r.Jobs, r.GBPerHour)
		}
	}
	if c != nil {
		printBilling(c)
	}
}
