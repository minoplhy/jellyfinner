package orchestrator

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
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
	return s.addNode(spec, false)
}

// addCloudNode registers a rented server's node, the one kind of node whose
// name starts with cloud-.
func (s *Service) addCloudNode(name string) (NodeSecrets, error) {
	return s.addNode(NodeSpec{Name: name}, true)
}

func (s *Service) addNode(spec NodeSpec, rented bool) (NodeSecrets, error) {
	var out NodeSecrets
	if err := checkNodeName(spec.Name, rented); err != nil {
		return out, err
	}
	prefixes, err := config.ParsePrefixes(spec.Allow)
	if err != nil {
		return out, err
	}
	token, err := nodes.NewSecret()
	if err != nil {
		return out, err
	}
	id, err := nodes.NewID()
	if err != nil {
		return out, err
	}
	node := model.Node{ID: id, Name: spec.Name, Kind: model.NodeWorker, TokenHash: nodes.HashToken(token), Allow: prefixes}
	if spec.Edge {
		if node.URL, err = edgeURL(spec.URL); err != nil {
			return out, err
		}
		if spec.QuotaGB <= 0 {
			return out, errors.New("an edge node needs a positive quota")
		}
		node.Kind, node.QuotaBytes = model.NodeEdge, spec.QuotaGB*1e9
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

func checkNodeName(name string, rented bool) error {
	if !nodeName.MatchString(name) {
		return errors.New("names use lowercase letters, digits and dashes")
	}
	if !rented && strings.HasPrefix(name, cloudNodePrefix) {
		return fmt.Errorf("names starting with %s are kept for rented servers", cloudNodePrefix)
	}
	return nil
}

func edgeURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", errors.New("an edge node needs a URL with an https address")
	}
	return strings.TrimSuffix(raw, "/"), nil
}

// NodeEdit is what to change on a node. A nil field stays as it is.
type NodeEdit struct {
	Name    *string
	Allow   *string // comma-separated CIDR; empty accepts any address
	URL     *string // edge only
	QuotaGB *int64  // edge only
}

// Rented reports whether a node belongs to a rented server, which the cloud
// scaler names and manages, so it cannot be edited.
func Rented(n *model.Node) bool {
	return !n.Edge() && strings.HasPrefix(n.Name, cloudNodePrefix)
}

// EditNode changes a node's name, allowed addresses, and for an edge node its
// URL and quota. Its ID, kind, token and signing key stay, so the node needs
// no change on its side. The warnings are worth showing but stop nothing.
// Serve and the router pick the change up within seconds.
func (s *Service) EditNode(id string, e NodeEdit) (model.Node, []string, error) {
	n, err := s.NodeByID(id)
	if err != nil {
		return model.Node{}, nil, err
	}
	if e.Name == nil && e.Allow == nil && e.URL == nil && e.QuotaGB == nil {
		return *n, nil, errors.New("nothing to change")
	}
	if Rented(n) {
		return *n, nil, fmt.Errorf("%s is a rented server's node, which the cloud scaler manages", n.Name)
	}
	if !n.Edge() && (e.URL != nil || e.QuotaGB != nil) {
		return *n, nil, errors.New("a URL and a quota are only for edge nodes")
	}
	old := *n
	var warnings []string
	if e.Name != nil && *e.Name != n.Name {
		if err := checkNodeName(*e.Name, false); err != nil {
			return old, nil, err
		}
		n.Name = *e.Name
		if n.Edge() {
			for user, edge := range s.Cfg.EdgeViewers {
				if edge == old.Name {
					warnings = append(warnings, fmt.Sprintf("EDGE_VIEWERS still sends %s to %s: change it to %s", user, old.Name, n.Name))
				}
			}
			sort.Strings(warnings)
		}
	}
	if e.Allow != nil {
		if n.Allow, err = config.ParsePrefixes(*e.Allow); err != nil {
			return old, nil, err
		}
	}
	if e.URL != nil {
		if n.URL, err = edgeURL(*e.URL); err != nil {
			return old, nil, err
		}
	}
	if e.QuotaGB != nil {
		if *e.QuotaGB <= 0 {
			return old, nil, errors.New("an edge node needs a positive quota")
		}
		n.QuotaBytes = *e.QuotaGB * 1e9
		used, err := s.Store.NodeUsed(n.ID)
		if err != nil {
			return old, nil, err
		}
		if used > n.QuotaBytes {
			warnings = append(warnings, fmt.Sprintf("%s already holds %.1f GB, more than the new quota. Nothing is removed, and no new title is cached there until it holds less",
				n.Name, float64(used)/1e9))
		}
	}
	if err := s.Nodes.Update(n); err != nil {
		return old, nil, err
	}
	return *n, warnings, nil
}

// PauseNode stops a worker, by ID, taking new jobs. A job it already holds
// runs to the end, so nothing is lost.
func (s *Service) PauseNode(id string) error { return s.setPaused(id, true) }

// ResumeNode lets a paused worker, by ID, take jobs again.
func (s *Service) ResumeNode(id string) error { return s.setPaused(id, false) }

func (s *Service) setPaused(id string, paused bool) error {
	n, err := s.NodeByID(id)
	if err != nil {
		return err
	}
	if n.Edge() {
		return fmt.Errorf("%s is an edge node, which takes no jobs, so it cannot be paused", n.Name)
	}
	return s.Nodes.SetPaused(id, paused)
}

// Requeue takes a running job from its worker and queues it again, for any
// worker to claim. With pause, the worker is paused first, so it cannot claim
// the job straight back; a paused cloud server drains and is deleted. If the
// job ends in between, the pause stays and the error says so.
func (s *Service) Requeue(id string, pause bool) (string, error) {
	if pause {
		j, err := s.Store.Get(id)
		if err != nil {
			return "", err
		}
		if j.State != model.StateRunning || j.Node == "" {
			return "", fmt.Errorf("job %s is %s, not running", id, j.State)
		}
		if err := s.PauseNode(j.Node); err != nil {
			return "", err
		}
	}
	node, err := s.Store.Requeue(id)
	if err != nil {
		return "", err
	}
	name := s.NodeName(node)
	s.Log.Printf("requeued job %s from %s", id, name)
	return name, nil
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
		if err := s.Nodes.SetPaused(n.ID, paused); err != nil {
			return names, err
		}
		names = append(names, n.Name)
	}
	return names, nil
}

// FindNode finds a live node by name. It reads straight from the database, so
// a node added a moment ago by another process is found.
func (s *Service) FindNode(name string) (*model.Node, error) {
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

// NodeByID finds a live node by ID, straight from the database.
func (s *Service) NodeByID(id string) (*model.Node, error) {
	all, err := s.Store.LoadNodes()
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == id {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("node %s: %w", id, model.ErrNotFound)
}

// NodeName is a node's name now, or the last it had if removed. An unknown ID
// is returned as it is.
func (s *Service) NodeName(id string) string {
	if n := s.Nodes.ByID(id); n != nil {
		return n.Name
	}
	if names, err := s.Store.NodeNames(); err == nil && names[id] != "" {
		return names[id]
	}
	return id
}

// RotateNodeToken issues a new token for a node, by ID. The old one stops
// working within seconds.
func (s *Service) RotateNodeToken(id string) (string, error) {
	token, err := nodes.NewSecret()
	if err != nil {
		return "", err
	}
	if err := s.Nodes.SetToken(id, nodes.HashToken(token)); err != nil {
		return "", err
	}
	return token, nil
}
