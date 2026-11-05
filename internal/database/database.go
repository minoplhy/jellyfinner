// Package database is the one door to persistent state. Store lists every
// operation the rest of the program may perform; a backend package such as
// database/sqlite implements it and registers itself under a driver name.
//
// Rules that keep the layout consistent:
//   - only a backend package imports database/sql or a SQL driver;
//   - everything else uses Store, or one of its parts, and the model types;
//   - only cmd/* chooses a backend, by importing it and calling Open.
//
// Each method is one atomic step. Where a step must read and write together,
// such as Claim or Fail, the backend runs it in one transaction, so callers
// never see a transaction type. "No such row" is reported as model.ErrNotFound.
package database

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"jellyfinner/internal/model"
)

// Options are the policies a backend applies inside its transactions.
type Options struct {
	// Now is the clock for every timestamp. Nil means time.Now.
	Now func() time.Time
	// RetryDelay spaces out attempts, growing with each one, so a short
	// outage cannot use up a job's attempts in seconds.
	RetryDelay time.Duration
	// BackfillBudget caps the source bytes backfill may start per UTC day,
	// and 0 means no limit.
	BackfillBudget int64
}

// Clock returns Now, or time.Now when unset.
func (o Options) Clock() func() time.Time {
	if o.Now == nil {
		return time.Now
	}
	return o.Now
}

// MetaStore keeps small named values.
type MetaStore interface {
	// Meta returns a stored value, or "" if unset.
	Meta(key string) (string, error)
	// SetMeta stores a value.
	SetMeta(key, value string) error
	// EnsureMeta stores fresh only if the key is unset, and returns whatever
	// is stored afterwards.
	EnsureMeta(key, fresh string) (string, error)
}

// JobStore is the encode queue and its leases.
type JobStore interface {
	// Enqueue inserts a queued job. It reports false if the ID already
	// exists, whatever its state, so the watcher cannot endlessly requeue a
	// failed encode.
	Enqueue(j *model.Job) (bool, error)
	// Get returns one job.
	Get(id string) (*model.Job, error)
	// List returns the most recent jobs, active ones first.
	List(limit int) ([]*model.Job, error)
	// ListFiltered returns one page of matching jobs, ordered as List orders
	// them, and how many match in all.
	ListFiltered(f model.JobFilter, limit, offset int) ([]*model.Job, int, error)
	// FailedByNode counts, per node, the jobs that failed for good since a time.
	FailedByNode(since time.Time) (map[string]int, error)
	// Publishing returns jobs waiting to be published, oldest first.
	Publishing() ([]*model.Job, error)
	// JobsByID returns every job's state.
	JobsByID() (map[string]string, error)
	// LatestJobs returns the newest job for each source path.
	LatestJobs() (map[string]*model.Job, error)
	// RunningByNode counts each node's running jobs.
	RunningByNode() (map[string]int, error)
	// WorkByNode sums, per node, the jobs it finished since a time.
	WorkByNode(since time.Time) (map[string]model.NodeWork, error)
	// Waiting describes the queued jobs a worker could claim now. Backfill
	// jobs count only when includeBackfill is set and backfill is running.
	Waiting(includeBackfill bool) (model.QueueWait, error)
	// Outcomes returns jobs that were published, failed or were cancelled in
	// [from, to), for usage history.
	Outcomes(from, to time.Time) ([]model.JobOutcome, error)
	// FirstActivity returns when the first job was queued, or 0.
	FirstActivity() (int64, error)
	// StateCounts returns the number of jobs in each state.
	StateCounts() (map[string]int, error)
	// HasActive reports whether a source has a job queued, running or
	// waiting to publish.
	HasActive(path string) (bool, error)

	// Claim leases the highest-priority queued job to a node, or returns
	// nil. A backfill job is only claimed while backfill is active and, with
	// a budget, while today's budget allows it; its size is charged then.
	Claim(node string, leaseFor time.Duration) (*model.Job, error)
	// Heartbeat extends a lease and records progress. It reports true when
	// the job was cancelled and the worker must stop.
	Heartbeat(id, lease, stage string, fraction float64, leaseFor time.Duration) (bool, error)
	// Complete moves a running job to publishing.
	Complete(id, lease, manifest, log string) error
	// Release requeues a job its worker gave up, giving the attempt back.
	Release(id, lease string) error
	// Requeue takes a running job from its worker and queues it again without
	// spending an attempt, returning the node it came from. Only a running job
	// can be requeued.
	Requeue(id string) (string, error)
	// Fail records a worker failure. The job is retried after RetryDelay
	// times its attempts until maxAttempts, and true is returned once it has
	// failed for good.
	Fail(id, lease, reason, log string, maxAttempts int) (bool, error)
	// ExpireLeases requeues running jobs whose worker went silent, and
	// returns those that have now failed for good.
	ExpireLeases(maxAttempts int) ([]*model.Job, error)
	// MarkDone records a successful publish.
	MarkDone(id string) error
	// PublishFailed requeues a job whose upload was rejected, and reports
	// true once it has failed for good.
	PublishFailed(id, reason string, maxAttempts int) (bool, error)
	// Cancel stops a queued or running job.
	Cancel(id string) error
	// CancelForPath cancels every unfinished job for a source, except keep.
	CancelForPath(path, keep string) (int64, error)
	// CancelStaleProfile cancels queued jobs built for another output profile.
	CancelStaleProfile(current string) (int64, error)
	// Bump raises a queued job to the new-title priority.
	Bump(id string) error
	// Retry requeues a failed or cancelled job with fresh attempts.
	Retry(id string) error
	// BudgetUsedToday returns the source bytes backfill has started today.
	BudgetUsedToday() (int64, error)
}

// SourceStore tracks files under the originals directory.
type SourceStore interface {
	// Sources returns every tracked source, ordered by path.
	Sources() ([]*model.Source, error)
	// Source returns one tracked source.
	Source(path string) (*model.Source, error)
	// AddSource starts tracking a file.
	AddSource(path string, size, mtimeNS int64, backlog bool) error
	// UpdateSource records a changed file, restarts its stability wait and
	// takes it out of the backlog.
	UpdateSource(path string, size, mtimeNS int64) error
	// SetBacklog holds a source for the backfill instead of queueing it.
	SetBacklog(path string) error
	// SetMissing records when a file disappeared, or clears it with 0.
	SetMissing(path string, since int64) error
	// RenameSource moves a row to a new path, keeping its encode record.
	RenameSource(from, to string) error
	// DeleteSource stops tracking a file.
	DeleteSource(path string) error
	// SetEncoded records the source version that was just published.
	SetEncoded(path string, size, mtimeNS int64, profile, sha256 string, bytes int64) error
	// BumpRevision increments and returns a source's revision.
	BumpRevision(path string) (int, error)
}

// RequestStore remembers which viewers asked for a title.
type RequestStore interface {
	// AddRequest records a viewer's request once per title.
	AddRequest(r model.Request) error
	// TakeRequests returns a title's requests and removes them.
	TakeRequests(path string) ([]model.Request, error)
	// RequestedPaths returns every title with a request still open.
	RequestedPaths() (map[string]bool, error)
}

// NodeStore keeps the nodes allowed on the control API. It does no caching;
// see the nodes package for that.
type NodeStore interface {
	// AddNode stores a new node, or returns model.ErrNodeExists when a live
	// node has its name.
	AddNode(n *model.Node) error
	// UpdateNode saves a live node's name, allowed addresses, URL and quota,
	// found by ID. A name another live node has returns model.ErrNodeExists.
	UpdateNode(n *model.Node) error
	// RemoveNode revokes a node and deletes its placements and status. The
	// node keeps its name in history, and the name is free again.
	RemoveNode(id string) error
	// SetNodeToken replaces a node's token hash.
	SetNodeToken(id, tokenHash string) error
	// SetNodePaused pauses or resumes a node. Claim gives a paused node no
	// job; one it already holds runs to the end.
	SetNodePaused(id string, paused bool) error
	// LoadNodes reads every live node, by name.
	LoadNodes() ([]model.Node, error)
	// NodeNames maps every node ID to its name, removed nodes included.
	NodeNames() (map[string]string, error)

	// SetViewer sends a Jellyfin user to an edge node, by ID, moving them
	// from any other. Usernames match without regard to case.
	SetViewer(username, node string) error
	// RemoveViewer sends a user back to the origin, or returns
	// model.ErrNotFound.
	RemoveViewer(username string) error
	// Viewers lists every assignment, by username. RemoveNode deletes a
	// node's assignments.
	Viewers() ([]model.Viewer, error)
}

// PlacementStore tracks encoded files on edge nodes, and what the nodes report.
type PlacementStore interface {
	// AddPlacement records a wanted file. A failed row for the same file is
	// replaced, so an operator can simply try again.
	AddPlacement(p *model.Placement) error
	// Placements lists every placement, by node and output.
	Placements() ([]*model.Placement, error)
	// NodePlacements lists one node's placements.
	NodePlacements(node string) ([]*model.Placement, error)
	// OutputPlacements lists the placements of one library path.
	OutputPlacements(output string) ([]*model.Placement, error)
	// ActivePlacement returns the servable copy of a title on a node.
	ActivePlacement(output, node string) (*model.Placement, error)
	// PullingPlacement returns a file a node has been ordered to pull.
	PullingPlacement(node, sha256 string) (*model.Placement, error)
	// SetPlacementState moves a placement to a new state.
	SetPlacementState(node, sha256, state, message string) error
	// FailPlacement records a pull that failed, and reports whether it was
	// still wanted.
	FailPlacement(node, sha256, message string) (bool, error)
	// ActivatePlacement makes a pulled file servable, unless it was
	// withdrawn meanwhile.
	ActivatePlacement(node, sha256 string) error
	// DeletePlacement forgets a placement.
	DeletePlacement(node, sha256 string) error
	// TouchPlacement records that a viewer was sent to this copy.
	TouchPlacement(node, sha256 string) error
	// RenamePlacements follows a title to a new library path.
	RenamePlacements(from, to string) error
	// NodeUsed returns the bytes a node holds or is pulling.
	NodeUsed(node string) (int64, error)
	// NodeSeen records an agent's report.
	NodeSeen(node string, free, total int64) error
	// SetNodeHealth records the result of a probe from the orchestrator.
	SetNodeHealth(node string, healthy bool) error
	// GetNodeStatus returns a node's status, zero if never heard from.
	GetNodeStatus(node string) (model.NodeStatus, error)
}

// ModeStore changes the encode mode.
type ModeStore interface {
	// SwitchMode changes the active mode in one transaction. Going to auto
	// moves unencoded titles to the backlog; going to manual cancels queued
	// work and pauses backfill. It returns how many rows changed.
	SwitchMode(to string) (int64, error)
}

// Store is every persistence operation.
// CloudStore keeps rented servers, including deleted ones as spending history.
type CloudStore interface {
	// AddCloudServer stores a new server.
	AddCloudServer(c *model.CloudServer) error
	// UpdateCloudServer saves every field of the server with this provider and ID.
	UpdateCloudServer(c *model.CloudServer) error
	// CloudServers returns live servers, and deleted ones whose deletion was
	// at or after since, oldest first.
	CloudServers(since time.Time) ([]*model.CloudServer, error)
}

type Store interface {
	MetaStore
	CloudStore
	JobStore
	SourceStore
	RequestStore
	NodeStore
	PlacementStore
	ModeStore
	Close() error
}

// OpenFunc opens a backend. dsn is backend specific: a file path for sqlite.
type OpenFunc func(dsn string, opts Options) (Store, error)

var (
	driversMu sync.Mutex
	drivers   = map[string]OpenFunc{}
)

// Register makes a backend available to Open. It panics on a duplicate name,
// which is a build mistake.
func Register(name string, open OpenFunc) {
	driversMu.Lock()
	defer driversMu.Unlock()
	if _, dup := drivers[name]; dup {
		panic("database: Register called twice for " + name)
	}
	drivers[name] = open
}

// Open opens a store with a registered backend.
func Open(driver, dsn string, opts Options) (Store, error) {
	driversMu.Lock()
	open, ok := drivers[driver]
	driversMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("database: unknown driver %q (have %v)", driver, Drivers())
	}
	return open(dsn, opts)
}

// Drivers returns the registered backend names, sorted.
func Drivers() []string {
	driversMu.Lock()
	defer driversMu.Unlock()
	names := make([]string, 0, len(drivers))
	for name := range drivers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
