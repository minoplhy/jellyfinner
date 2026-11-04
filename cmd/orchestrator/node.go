package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"

	"jellyfinner/internal/config"
	"jellyfinner/internal/queue"
)

var nodeName = regexp.MustCompile(`^[a-z0-9-]+$`)

// nodeCommand changes the nodes table. serve and the router pick up a change
// within seconds, so none of this needs a restart.
func nodeCommand(envFile string, args []string) error {
	if len(args) == 0 {
		return errors.New("node needs add, list, remove or rotate")
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
	name := rest[0]
	switch command {
	case "add":
		return addNode(cfg, store, name, rest[1:])
	case "remove":
		return store.RemoveNode(name)
	case "rotate":
		token, err := queue.NewSecret()
		if err != nil {
			return err
		}
		if err := store.SetNodeToken(name, queue.HashToken(token)); err != nil {
			return err
		}
		fmt.Printf("New token for %s. The old one stops working within seconds.\n\n      NODE_TOKEN: %s\n", name, token)
		return nil
	}
	return fmt.Errorf("unknown node command %q", command)
}

func addNode(cfg *config.Config, store *queue.Store, name string, args []string) error {
	fs := flag.NewFlagSet("node add", flag.ExitOnError)
	isEdge := fs.Bool("edge", false, "this node caches encodes for viewers")
	edgeURL := fs.String("url", "", "edge only: the node's public https URL")
	quotaGB := fs.Int64("quota-gb", 0, "edge only: disk space to use")
	allow := fs.String("allow", "", "source addresses to accept, comma-separated CIDR")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !nodeName.MatchString(name) {
		return errors.New("names use lowercase letters, digits and dashes")
	}
	prefixes, err := config.ParsePrefixes(*allow)
	if err != nil {
		return fmt.Errorf("--allow: %w", err)
	}
	token, err := queue.NewSecret()
	if err != nil {
		return err
	}
	node := &queue.Node{Name: name, Kind: queue.NodeWorker, TokenHash: queue.HashToken(token), Allow: prefixes}
	if *isEdge {
		u, err := url.Parse(*edgeURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("an edge node needs --url with an https address")
		}
		if *quotaGB <= 0 {
			return errors.New("an edge node needs a positive --quota-gb")
		}
		node.Kind, node.URL, node.QuotaBytes = queue.NodeEdge, strings.TrimSuffix(*edgeURL, "/"), *quotaGB*1e9
		if node.SigningKey, err = queue.NewSecret(); err != nil {
			return err
		}
	} else if *edgeURL != "" || *quotaGB != 0 {
		return errors.New("--url and --quota-gb need --edge")
	}
	if err := store.AddNode(node); err != nil {
		return err
	}

	control := cfg.ControlURL
	if control == "" {
		control = "https://control.example.com"
		fmt.Println("CONTROL_URL is not set here, so replace the address below with the one nodes reach.")
	}
	fmt.Printf("Added %s. It works now, with no restart. The token is shown only this once.\n\n", name)
	if node.Edge() {
		fmt.Printf("In the edge compose file, under edge-pull, environment:\n\n      ORCHESTRATOR_URL: %s\n      NODE_TOKEN: %s\n\n", control, token)
		fmt.Printf("Under edge-serve, environment:\n\n      EDGE_SIGNING_KEY: %s\n", node.SigningKey)
		return nil
	}
	fmt.Printf("In the worker compose file, under worker, environment:\n\n      ORCHESTRATOR_URL: %s\n      NODE_TOKEN: %s\n", control, token)
	return nil
}

func listNodes(store *queue.Store) error {
	nodes, err := store.LoadNodes()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tKIND\tQUOTA GB\tURL\tALLOW")
	for _, n := range nodes {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%v\n", n.Name, n.Kind, n.QuotaBytes/1e9, n.URL, n.Allow)
	}
	return w.Flush()
}
