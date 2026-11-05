package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"jellyfinner/internal/cloud"
	"jellyfinner/internal/notify"
	"jellyfinner/internal/orchestrator"
)

func providerNames(providers []cloud.Provider) []string {
	names := make([]string, len(providers))
	for i, p := range providers {
		names[i] = p.Name()
	}
	return names
}

// cloudCommand manages rented workers. It works through the database, so a
// running serve picks up every change.
func cloudCommand(envFile string, args []string) error {
	if len(args) == 0 {
		return errors.New("cloud needs status, up, drain, down, pause, resume, reconcile or estimate")
	}
	cfg, store, err := open(envFile)
	if err != nil {
		return err
	}
	defer store.Close()
	svc := orchestrator.New(cfg, store, notify.New(cfg.DiscordURL, cfg.NtfyURL), nil, log.New(os.Stderr, "", 0))
	providers, err := orchestrator.BuildProviders(cfg.Cloud)
	if err != nil {
		return err
	}
	c, err := orchestrator.NewCloud(svc, providers, nil)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	command, rest := args[0], args[1:]
	switch command {
	case "status":
		return cloudStatus(ctx, c)
	case "pause", "resume":
		if err := c.SetPaused(command == "pause"); err != nil {
			return err
		}
		if command == "pause" {
			fmt.Println("Paused: no new cloud server starts. Running ones finish their jobs and are deleted as they go idle.")
		} else {
			fmt.Println("Resumed: cloud servers start again when the queue needs them.")
		}
		return nil
	case "drain":
		if len(rest) != 1 {
			return errors.New("cloud drain needs a server NAME, from cloud status")
		}
		if err := c.Drain(rest[0]); err != nil {
			return err
		}
		fmt.Printf("Draining %s: it finishes its job, then serve deletes it.\n", rest[0])
		return nil
	case "down":
		return cloudDown(ctx, c, rest)
	case "reconcile":
		if err := c.Reconcile(ctx); err != nil {
			return err
		}
		fmt.Println("Reconciled with every listed provider.")
		return cloudStatus(ctx, c)
	case "up":
		fs := flag.NewFlagSet("cloud up", flag.ExitOnError)
		only := fs.String("provider", "", "try only this provider; otherwise CLOUD_PROVIDERS order")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		r, err := c.Start(ctx, *only)
		if err != nil {
			return err
		}
		fmt.Printf("Started %s: %s %s in %s, server %s. It calls in once booted; serve deletes it when idle.\n",
			r.Name, r.Provider, r.Type, r.Location, r.ID)
		return nil
	case "estimate":
		return cloudEstimate(ctx, c)
	}
	return fmt.Errorf("unknown cloud command %q", command)
}

func cloudDown(ctx context.Context, c *orchestrator.Cloud, args []string) error {
	fs := flag.NewFlagSet("cloud down", flag.ExitOnError)
	wait := fs.Bool("wait", false, "block until every delete is confirmed by its provider")
	timeout := fs.Duration("timeout", 30*time.Minute, "with --wait, how long to wait")
	if err := fs.Parse(args); err != nil {
		return err
	}
	n, err := c.DrainAll()
	if err != nil {
		return err
	}
	fmt.Printf("Draining %d servers: each finishes its job, then is deleted.\n", n)
	if !*wait {
		return nil
	}
	deadline := time.Now().Add(*timeout)
	for {
		if err := c.Advance(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "  ", err)
		}
		servers, err := c.Servers()
		if err != nil {
			return err
		}
		if len(servers) == 0 {
			fmt.Println("Every server is deleted, confirmed by its provider.")
			return nil
		}
		var left []string
		for _, s := range servers {
			left = append(left, s.Name+" ("+s.State+")")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not confirmed deleted after %s: %s. Check each provider's console", *timeout, strings.Join(left, ", "))
		}
		fmt.Printf("  waiting for %s\n", strings.Join(left, ", "))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

func cloudStatus(ctx context.Context, c *orchestrator.Cloud) error {
	paused, err := c.Paused()
	if err != nil {
		return err
	}
	if len(c.Providers()) == 0 {
		fmt.Println("No cloud providers are listed in CLOUD_PROVIDERS.")
	} else {
		state := "on"
		if paused {
			state = "paused: no new servers"
		}
		fmt.Printf("Cloud workers %s. Providers in your order: %s\n", state, strings.Join(providerNames(c.Providers()), ", "))
	}
	servers, err := c.Servers()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "\nSERVER\tPROVIDER\tTYPE\tLOCATION\tSTATE\tAGE\tPAID UNTIL\tCOST SO FAR")
	for _, s := range servers {
		state := s.State
		if s.Overdue {
			state += ", DELETE NOT CONFIRMED"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, s.Provider, s.Type, s.Location, state,
			time.Since(time.Unix(s.Created, 0)).Round(time.Minute), s.PaidUntil.Local().Format("15:04"), money(s.Cost, s.Currency))
	}
	if len(servers) == 0 {
		fmt.Fprintln(w, "(none running)")
	}
	if err := w.Flush(); err != nil {
		return err
	}
	usage, err := c.Usage(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\nBilled hours today (UTC): %.0f of %.0f allowed.\n", usage.HoursToday, svcHoursCap(c))
	for _, s := range usage.Spend {
		fmt.Printf("  %s: about %s today, %s this month\n", s.Provider, money(s.Today, s.Currency), money(s.Month, s.Currency))
	}
	printBilling(c)
	return nil
}

func svcHoursCap(c *orchestrator.Cloud) float64 { return c.HoursCap() }

func printBilling(c *orchestrator.Cloud) {
	if len(c.Providers()) == 0 {
		return
	}
	fmt.Println("\nEstimates exclude VAT and traffic, using prices quoted when each server started. Billing rules:")
	for _, b := range c.Billing() {
		fmt.Printf("  %s: %s\n", b.Name, b.Billing.Note)
		for _, s := range b.Billing.Sources {
			fmt.Printf("    %s, %s (checked %s)\n", s.Title, s.URL, b.Billing.Checked)
		}
	}
}

func cloudEstimate(ctx context.Context, c *orchestrator.Cloud) error {
	if len(c.Providers()) == 0 {
		return errors.New("no cloud providers are listed in CLOUD_PROVIDERS")
	}
	est, err := c.Estimate(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Queue: %d titles, %.1f GB of source.\n\n", est.QueueTitles, est.QueueGB)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tTYPE\tLOCATION\tPER HOUR\tMEASURED\tGB PER HOUR\tPER TITLE\tTO CLEAR QUEUE")
	measured := false
	for _, r := range est.Rows {
		perHour := "price unavailable"
		if r.Currency != "" {
			perHour = money(r.Hourly, r.Currency)
			if r.Taxed {
				perHour += " incl. tax"
			}
		}
		basis, gb, title, queue := "no jobs yet", "-", "-", "-"
		if r.Jobs > 0 {
			measured = true
			basis = fmt.Sprintf("%d jobs", r.Jobs)
			gb = fmt.Sprintf("%.1f", r.GBPerHour)
			title = money(r.CostPerTitle, r.Currency)
			if r.QueueHours > 0 {
				queue = fmt.Sprintf("%s, about %.1f h", money(r.QueueCost, r.Currency), r.QueueHours)
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Provider, r.Type, r.Location, perHour, basis, gb, title, queue)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if !measured {
		fmt.Println("\nOnly prices so far: speed is measured from real jobs, not guessed, so per-title and queue costs appear after servers have finished work.")
	}
	if len(est.Rows) > 0 && !est.Rows[0].PricedAt.IsZero() {
		fmt.Printf("\nPrices fetched %s from each provider's API. Rows follow your CLOUD_PROVIDERS order; prices never change it.\n",
			est.Rows[0].PricedAt.Local().Format("2006-01-02 15:04"))
	}
	printBilling(c)
	return nil
}

func money(v float64, currency string) string {
	if currency == "" {
		return "unknown"
	}
	return fmt.Sprintf("%.4f %s", v, currency)
}
