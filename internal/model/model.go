// Package model holds the orchestrator's records: encode jobs, tracked sources,
// nodes, edge placements and viewer requests. It has no storage code; the
// database package persists these types.
package model

import (
	"errors"
	"net/netip"
)

// Job states.
const (
	StateQueued     = "queued"
	StateRunning    = "running"
	StatePublishing = "publishing"
	StateDone       = "done"
	StateFailed     = "failed"
	StateCancelled  = "cancelled"
)

// Priorities. New titles always run before the archive.
const (
	PriorityBackfill = 0
	PriorityNew      = 10
)

// Node kinds. A node has exactly one, so an edge token can never take a job.
const (
	NodeWorker = "worker"
	NodeEdge   = "edge"
)

// Placement states. Only an active placement is ever served.
const (
	PlacementPulling  = "pulling"
	PlacementActive   = "active"
	PlacementDeleting = "deleting"
	PlacementFailed   = "failed"
)

// Meta keys the orchestrator keeps in the database.
const (
	MetaActiveMode       = "active_mode"
	MetaBackfillActive   = "backfill_active"
	MetaBackfillFinished = "backfill_finished"
	MetaFirstScanDone    = "first_scan_done"
	// MetaBackfillUsedPrefix is followed by a UTC day, 2006-01-02.
	MetaBackfillUsedPrefix = "backfill_used:"
)

// Errors every database backend returns.
var (
	// ErrLeaseLost means the caller no longer owns the job.
	ErrLeaseLost = errors.New("job lease lost")
	// ErrNotFound means no row matched.
	ErrNotFound = errors.New("job not found")
	// ErrNodeExists means the name is taken.
	ErrNodeExists = errors.New("node already exists")
)

// Job is one encode in the queue.
type Job struct {
	ID          string  `json:"id"`
	Path        string  `json:"path"`
	OutputPath  string  `json:"output_path"`
	SourceSize  int64   `json:"source_size"`
	SourceMtime int64   `json:"source_mtime"`
	Revision    int     `json:"revision"`
	Profile     string  `json:"profile"`
	Priority    int     `json:"priority"`
	State       string  `json:"state"`
	Attempts    int     `json:"attempts"`
	LeaseID     string  `json:"-"`
	Node        string  `json:"node"`
	Stage       string  `json:"stage"`
	Fraction    float64 `json:"fraction"`
	Reason      string  `json:"reason"`
	ForceEncode bool    `json:"force_encode"`
	Software    bool    `json:"software"`
	Error       string  `json:"error"`
	Log         string  `json:"-"`
	Manifest    string  `json:"-"`
	Created     int64   `json:"created"`
	Updated     int64   `json:"updated"`
}

// JobFilter narrows a job listing. Empty fields match everything. Search
// matches part of the path, ignoring ASCII case.
type JobFilter struct {
	State  string
	Node   string
	Search string
}

// Source is one file tracked under the originals directory. The Encoded fields
// record the version that was last published.
type Source struct {
	Path           string
	Size           int64
	MtimeNS        int64
	LastChanged    int64
	Backlog        bool
	Revision       int
	EncodedSize    int64
	EncodedMtimeNS int64
	EncodedProfile string
	EncodedSHA256  string
	EncodedBytes   int64
	MissingSince   int64
}

// Encoded reports whether the current version has been published.
func (s *Source) Encoded() bool {
	return s.EncodedSize == s.Size && s.EncodedMtimeNS == s.MtimeNS
}

// Request records who asked for a title, so they can be told when it is ready
// and the played flag set by the stub can be cleared.
type Request struct {
	Path     string
	UserID   string
	Username string
	ItemID   string
}

// Node is a worker or edge node allowed on the control API. An empty Allow list
// accepts any source address. URL, QuotaBytes and SigningKey are edge only.
type Node struct {
	Name       string
	Kind       string
	TokenHash  string
	Allow      []netip.Prefix
	URL        string
	QuotaBytes int64
	SigningKey string
}

// Edge reports whether this node caches encodes.
func (n *Node) Edge() bool { return n.Kind == NodeEdge }

// Placement is one encoded file on one edge node. Files are identified by
// content hash, so a re-encode can never be served from a stale copy.
type Placement struct {
	Node       string
	SHA256     string
	Output     string
	Size       int64
	State      string
	Error      string
	LastPlayed int64
}

// NodeStatus is what the orchestrator last learned about a node.
type NodeStatus struct {
	Node      string
	LastSeen  int64
	Healthy   bool
	DiskFree  int64
	DiskTotal int64
}
