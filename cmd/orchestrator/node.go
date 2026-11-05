package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"text/tabwriter"

	"jellyfinner/internal/database"
	"jellyfinner/internal/model"
	"jellyfinner/internal/notify"
	"jellyfinner/internal/orchestrator"
)

// nodeCommand changes the nodes table. serve and the router pick up a change
// within seconds, so none of this needs a restart.
func nodeCommand(envFile string, args []string) error {
	if len(args) == 0 {
		return errors.New("node needs add, list, edit, remove, rotate, pause or resume")
	}
	cfg, store, err := open(envFile)
	if err != nil {
		return err
	}
	defer store.Close()
	command, rest := args[0], args[1:]
	if command == "list" {
		return listNodes(store)
	}
	if len(rest) == 0 {
		return fmt.Errorf("node %s needs a NAME", command)
	}
	svc := orchestrator.New(cfg, store, notify.New("", ""), nil, log.Default())
	if command == "pause" || command == "resume" {
		return pauseNodes(svc, command == "pause", rest[0])
	}
	name := rest[0]
	if command == "add" {
		return addNode(svc, name, rest[1:])
	}
	n, err := svc.FindNode(name)
	if err != nil {
		return err
	}
	switch command {
	case "edit":
		return editNode(svc, n, rest[1:])
	case "remove":
		return svc.Nodes.Remove(n.ID)
	case "rotate":
		token, err := svc.RotateNodeToken(n.ID)
		if err != nil {
			return err
		}
		fmt.Printf("New token for %s. The old one stops working within seconds.\n\n      NODE_TOKEN: %s\n", name, token)
		return nil
	}
	return fmt.Errorf("unknown node command %q", command)
}

func addNode(svc *orchestrator.Service, name string, args []string) error {
	fs := flag.NewFlagSet("node add", flag.ExitOnError)
	isEdge := fs.Bool("edge", false, "this node caches encodes for viewers")
	isRouter := fs.Bool("router", false, "this node sits in front of Jellyfin and sends viewers to edge nodes")
	edgeURL := fs.String("url", "", "edge only: the node's public https URL")
	quotaGB := fs.Int64("quota-gb", 0, "edge only: disk space to use")
	allow := fs.String("allow", "", "source addresses to accept, comma-separated CIDR")
	if err := fs.Parse(args); err != nil {
		return err
	}
	added, err := svc.AddNode(orchestrator.NodeSpec{Name: name, Edge: *isEdge, Router: *isRouter, URL: *edgeURL, QuotaGB: *quotaGB, Allow: *allow})
	if err != nil {
		return err
	}
	control := svc.Cfg.ControlURL
	if control == "" {
		control = orchestrator.ControlURLPlaceholder
		fmt.Println("CONTROL_URL is not set here, so replace the address below with the one nodes reach.")
	}
	fmt.Printf("Added %s. It works now, with no restart. The token is shown only this once.\n\n", name)
	if added.Node.Edge() {
		fmt.Printf("In the edge compose file, under edge-pull, environment:\n\n      ORCHESTRATOR_URL: %s\n      NODE_TOKEN: %s\n\n", control, added.Token)
		fmt.Printf("Under edge-serve, environment:\n\n      EDGE_SIGNING_KEY: %s\n", added.SigningKey)
		return nil
	}
	if added.Node.Router() {
		fmt.Printf("In the router compose file, on the Jellyfin server, under router, environment:\n\n      ORCHESTRATOR_URL: %s\n      NODE_TOKEN: %s\n", control, added.Token)
		fmt.Println("\nAlso set JELLYFIN_URL, JELLYFIN_TOKEN and JELLYFIN_LIBRARY_DIR there. See deploy/docker/compose.router.yml.")
		return nil
	}
	fmt.Printf("In the worker compose file, under worker, environment:\n\n      ORCHESTRATOR_URL: %s\n      NODE_TOKEN: %s\n", control, added.Token)
	return nil
}

// editNode changes only what a flag is given for. --allow "" clears the list,
// so any address is accepted.
func editNode(svc *orchestrator.Service, n *model.Node, args []string) error {
	fs := flag.NewFlagSet("node edit", flag.ExitOnError)
	name := fs.String("name", "", "a new name")
	allow := fs.String("allow", "", "source addresses to accept, comma-separated CIDR; empty for any")
	edgeURL := fs.String("url", "", "edge only: the node's public https URL")
	quotaGB := fs.Int64("quota-gb", 0, "edge only: disk space to use")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var e orchestrator.NodeEdit
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "name":
			e.Name = name
		case "allow":
			e.Allow = allow
		case "url":
			e.URL = edgeURL
		case "quota-gb":
			e.QuotaGB = quotaGB
		}
	})
	edited, warnings, err := svc.EditNode(n.ID, e)
	if err != nil {
		return err
	}
	allowed := orchestrator.AllowString(edited.Allow)
	if allowed == "" {
		allowed = "any address"
	}
	fmt.Printf("Saved %s (ID %s). It takes effect within seconds, and nothing changes on the node.\n", edited.Name, edited.ID)
	fmt.Printf("  allowed: %s\n", allowed)
	if edited.Edge() {
		fmt.Printf("  url:     %s\n  quota:   %d GB\n", edited.URL, edited.QuotaBytes/1e9)
	}
	for _, w := range warnings {
		fmt.Printf("Note: %s.\n", w)
	}
	return nil
}

// pauseNodes takes one name, or --all for every worker. A paused worker
// finishes the job it holds and takes no more until resumed.
func pauseNodes(svc *orchestrator.Service, paused bool, name string) error {
	verb := map[bool]string{true: "Paused", false: "Resumed"}[paused]
	if name == "--all" {
		names, err := svc.SetAllWorkersPaused(paused)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			fmt.Println("There are no workers.")
			return nil
		}
		fmt.Printf("%s: %s\n", verb, strings.Join(names, ", "))
		return nil
	}
	n, err := svc.FindNode(name)
	if err != nil {
		return err
	}
	if paused {
		err = svc.PauseNode(n.ID)
	} else {
		err = svc.ResumeNode(n.ID)
	}
	if err != nil {
		return err
	}
	if paused {
		fmt.Printf("%s %s. A job it holds runs to the end, then it takes no more.\n", verb, name)
	} else {
		fmt.Printf("%s %s.\n", verb, name)
	}
	return nil
}

func listNodes(store database.Store) error {
	nodes, err := store.LoadNodes()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tID\tKIND\tSTATE\tQUOTA GB\tURL\tALLOW")
	for _, n := range nodes {
		state := "active"
		if n.Paused {
			state = "paused"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", n.Name, n.ID, n.Kind, state, n.QuotaBytes/1e9, n.URL, orchestrator.AllowString(n.Allow))
	}
	return w.Flush()
}
