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
	MetaCloudPaused      = "cloud_paused"
	MetaCloudCluster     = "cloud_cluster"
	// MetaBackfillUsedPrefix is followed by a UTC day, 2006-01-02.
	MetaBackfillUsedPrefix = "backfill_used:"
)

// Errors every database backend returns.
var (
	// ErrLeaseLost means the caller no longer owns the job.
	ErrLeaseLost = errors.New("job lease lost")
	// ErrNotFound means no row matched.
	ErrNotFound = errors.New("not found")
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
	Node        string  `json:"node"` // the node's ID
	NodeName    string  `json:"-"`    // the node's name now, or its last one if removed
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
	Started     int64   `json:"-"` // when the last attempt was claimed, 0 before timing existed
	Finished    int64   `json:"-"` // when the last attempt was handed back, 0 until then
}

// JobFilter narrows a job listing. Empty fields match everything. Search
// matches part of the path, ignoring ASCII case.
type JobFilter struct {
	State  string
	Node   string // a node ID
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
// ID never changes and is what every other record refers to; Name is what
// people type and see, and may be edited.
type Node struct {
	ID         string
	Name       string
	Kind       string
	TokenHash  string
	Allow      []netip.Prefix
	URL        string
	QuotaBytes int64
	SigningKey string
	Paused     bool // a paused worker claims no new job
}

// Edge reports whether this node caches encodes.
func (n *Node) Edge() bool { return n.Kind == NodeEdge }

// Viewer sends one Jellyfin user to an edge node, by the edge's ID. Usernames
// match without regard to case, as Jellyfin logins do.
type Viewer struct {
	Username string
	Node     string
}

// Placement is one encoded file on one edge node. Files are identified by
// content hash, so a re-encode can never be served from a stale copy.
type Placement struct {
	Node       string // ID
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

// Cloud server states, in lifecycle order. A server is billed in every state
// but CloudDeleted, which is only reached once the provider confirms it is gone.
const (
	CloudBooting  = "booting"  // created, not yet called the control API
	CloudReady    = "ready"    // taking jobs
	CloudDraining = "draining" // node paused, waiting for its job to end
	CloudDeleting = "deleting" // delete sent, not yet confirmed
	CloudDeleted  = "deleted"  // confirmed gone; kept as spending history
)

// CloudServer is one rented worker. Times are Unix seconds, zero when unset.
// The price is the one quoted at creation, so spending history stays fixed
// when the provider changes its prices later.
type CloudServer struct {
	Provider  string
	ID        string // the provider's own ID
	Name      string // the server's name at the provider, also its node's name
	Node      string // the node's ID
	Type      string
	Location  string
	Resources string // provider-specific extras to delete with it, such as an IP
	State     string
	Created   int64
	FirstSeen int64
	IdleSince int64
	DeleteAt  int64 // first delete request
	Deleted   int64
	Alerted   bool // the unconfirmed-delete alert was sent

	HourlyPrice float64 // excluding VAT, server plus extras
	Currency    string
}

// Live reports whether the server may still be billing.
func (c *CloudServer) Live() bool { return c.State != CloudDeleted }

// NodeWork is what one node finished over a period.
type NodeWork struct {
	Jobs        int
	SourceBytes int64
}

// JobOutcome is a finished, failed or cancelled job, for usage history.
type JobOutcome struct {
	Node       string // ID
	NodeName   string
	State      string
	SourceSize int64
	Attempts   int
	Started    int64
	Finished   int64
	Updated    int64
	Manifest   string
}

// Timed reports whether the job has a usable time for its last attempt.
func (o JobOutcome) Timed() bool { return o.Started > 0 && o.Finished > o.Started }

// QueueWait describes the jobs that are ready to claim now.
type QueueWait struct {
	Count       int
	SourceBytes int64
	Oldest      int64 // created time of the oldest, Unix seconds
}
