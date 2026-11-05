package orchestrator

import (
	"errors"
	"net/url"
	"regexp"
	"strings"

	"jellyfinner/internal/config"
	"jellyfinner/internal/queue"
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
	Node       queue.Node
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
	token, err := queue.NewSecret()
	if err != nil {
		return out, err
	}
	node := queue.Node{Name: spec.Name, Kind: queue.NodeWorker, TokenHash: queue.HashToken(token), Allow: prefixes}
	if spec.Edge {
		u, err := url.Parse(spec.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return out, errors.New("an edge node needs a URL with an https address")
		}
		if spec.QuotaGB <= 0 {
			return out, errors.New("an edge node needs a positive quota")
		}
		node.Kind, node.URL, node.QuotaBytes = queue.NodeEdge, strings.TrimSuffix(spec.URL, "/"), spec.QuotaGB*1e9
		if node.SigningKey, err = queue.NewSecret(); err != nil {
			return out, err
		}
	} else if spec.URL != "" || spec.QuotaGB != 0 {
		return out, errors.New("a URL and a quota are only for edge nodes")
	}
	if err := s.Store.AddNode(&node); err != nil {
		return out, err
	}
	return NodeSecrets{Node: node, Token: token, SigningKey: node.SigningKey}, nil
}

// RotateNodeToken issues a new token. The old one stops working within
// seconds.
func (s *Service) RotateNodeToken(name string) (string, error) {
	token, err := queue.NewSecret()
	if err != nil {
		return "", err
	}
	if err := s.Store.SetNodeToken(name, queue.HashToken(token)); err != nil {
		return "", err
	}
	return token, nil
}
