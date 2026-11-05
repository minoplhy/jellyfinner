package orchestrator

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"jellyfinner/internal/config"
	"jellyfinner/internal/model"
	"jellyfinner/internal/nodes"
)

var nodeName = regexp.MustCompile(`^[a-z0-9-]+$`)

// ControlURLPlaceholder stands in for CONTROL_URL when it is not set.
const ControlURLPlaceholder = "https://control.example.com"

// NodeSpec describes a node to add. URL and QuotaGB are for edge nodes only.
// Allow is a comma-separated list of CIDR prefixes, empty for any address.
type NodeSpec struct {
	Name    string
	Edge    bool
	URL     string
	QuotaGB int64
	Allow   string
}

// NodeSecrets is what a new node needs in its compose file. Only hashes of the
// token are stored, so this is the one time it can be shown.
type NodeSecrets struct {
	Node       model.Node
	Token      string
	SigningKey string // edge nodes only
}

// AddNode registers a node. Serve and the router pick it up within seconds,
// with no restart.
func (s *Service) AddNode(spec NodeSpec) (NodeSecrets, error) {
	var out NodeSecrets
	if !nodeName.MatchString(spec.Name) {
		return out, errors.New("names use lowercase letters, digits and dashes")
	}
	prefixes, err := config.ParsePrefixes(spec.Allow)
	if err != nil {
		return out, err
	}
	token, err := nodes.NewSecret()
	if err != nil {
		return out, err
	}
	node := model.Node{Name: spec.Name, Kind: model.NodeWorker, TokenHash: nodes.HashToken(token), Allow: prefixes}
	if spec.Edge {
		u, err := url.Parse(spec.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return out, errors.New("an edge node needs a URL with an https address")
		}
		if spec.QuotaGB <= 0 {
			return out, errors.New("an edge node needs a positive quota")
		}
		node.Kind, node.URL, node.QuotaBytes = model.NodeEdge, strings.TrimSuffix(spec.URL, "/"), spec.QuotaGB*1e9
		if node.SigningKey, err = nodes.NewSecret(); err != nil {
			return out, err
		}
	} else if spec.URL != "" || spec.QuotaGB != 0 {
		return out, errors.New("a URL and a quota are only for edge nodes")
	}
	if err := s.Nodes.Add(&node); err != nil {
		return out, err
	}
	return NodeSecrets{Node: node, Token: token, SigningKey: node.SigningKey}, nil
}

// PauseNode stops a worker taking new jobs. A job it already holds runs to the
// end, so nothing is lost.
func (s *Service) PauseNode(name string) error { return s.setPaused(name, true) }

// ResumeNode lets a paused worker take jobs again.
func (s *Service) ResumeNode(name string) error { return s.setPaused(name, false) }

func (s *Service) setPaused(name string, paused bool) error {
	n, err := s.findNode(name)
	if err != nil {
		return err
	}
	if n.Edge() {
		return fmt.Errorf("%s is an edge node, which takes no jobs, so it cannot be paused", name)
	}
	return s.Nodes.SetPaused(name, paused)
}

// SetAllWorkersPaused pauses or resumes every worker, and returns their names.
func (s *Service) SetAllWorkersPaused(paused bool) ([]string, error) {
	all, err := s.Store.LoadNodes()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range all {
		if n.Edge() {
			continue
		}
		if err := s.Nodes.SetPaused(n.Name, paused); err != nil {
			return names, err
		}
		names = append(names, n.Name)
	}
	return names, nil
}

// findNode reads straight from the database, so a node added a moment ago by
// another process is found.
func (s *Service) findNode(name string) (*model.Node, error) {
	all, err := s.Store.LoadNodes()
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("node %s: %w", name, model.ErrNotFound)
}

// RotateNodeToken issues a new token. The old one stops working within
// seconds.
func (s *Service) RotateNodeToken(name string) (string, error) {
	token, err := nodes.NewSecret()
	if err != nil {
		return "", err
	}
	if err := s.Nodes.SetToken(name, nodes.HashToken(token)); err != nil {
		return "", err
	}
	return token, nil
}
