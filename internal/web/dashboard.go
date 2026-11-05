package web

import (
	"net/http"
	"time"

	"jellyfinner/internal/config"
	"jellyfinner/internal/edgerouter"
	"jellyfinner/internal/model"
	"jellyfinner/internal/orchestrator"
)

// nodeState is how a node is doing. Class picks the badge colour: online,
// offline, unhealthy or unseen. Every page that shows a node uses
// workerState or edgeState, so no two pages can disagree.
type nodeState struct {
	Class string
	Label string
}

// workerState judges an encoding node by its last control API call. Those
// times live in memory, so a restart forgets them. A paused node says so
// whatever its connection, because it is why it takes no work.
func (s *Server) workerState(seen time.Time, paused bool) nodeState {
	st := nodeState{"offline", "offline"}
	switch {
	case seen.IsZero():
		st = nodeState{"unseen", "not seen since restart"}
	case s.now().Sub(seen) < 2*s.svc.Cfg.LeaseFor:
		st = nodeState{"online", "online"}
	}
	if paused {
		st = nodeState{"paused", "paused, " + st.Label}
	}
	return st
}

// edgeState uses the router's own rule, so "online" means viewers are sent
// there.
func (s *Server) edgeState(st model.NodeStatus) nodeState {
	switch {
	case st.LastSeen == 0:
		return nodeState{"unseen", "never reported"}
	case edgerouter.Servable(st, s.now()):
		return nodeState{"online", "online"}
	case s.now().Sub(time.Unix(st.LastSeen, 0)) < edgerouter.HealthWindow:
		return nodeState{"unhealthy", "unhealthy: probe failing"}
	}
	return nodeState{"offline", "not reporting"}
}

// dashNode is one line of the dashboard's node list.
type dashNode struct {
	Name  string
	Role  string
	State nodeState
	Seen  time.Time
}

// tile is one count on the dashboard, linking to the queue filtered by it.
type tile struct {
	State string
	Label string
	Count int
}

type dashboardData struct {
	CSRF       string
	Manual     bool
	Tiles      []tile
	Nodes      []dashNode
	Active     []*model.Job
	Backfill   orchestrator.BackfillStatus
	Budget     int64
	BudgetLow  int64
	BudgetHigh int64
}

func (s *Server) dashboardData(csrf string) (dashboardData, error) {
	budget := s.svc.Cfg.BackfillDailyBytes
	data := dashboardData{CSRF: csrf, Manual: s.svc.Cfg.Mode == config.ModeManual,
		Budget: budget, BudgetLow: budget / 100 * 80, BudgetHigh: budget / 100 * 95}
	seen := s.lastSeen()
	for _, node := range s.svc.Nodes.All() {
		row := dashNode{Name: node.Name, Role: "encoding", Seen: seen[node.Name]}
		if node.Edge() {
			st, err := s.svc.Store.GetNodeStatus(node.Name)
			if err != nil {
				return data, err
			}
			row.Role, row.State, row.Seen = "edge", s.edgeState(st), time.Time{}
			if st.LastSeen != 0 {
				row.Seen = time.Unix(st.LastSeen, 0)
			}
		} else {
			row.State = s.workerState(row.Seen, node.Paused)
		}
		data.Nodes = append(data.Nodes, row)
	}
	counts, err := s.svc.Store.StateCounts()
	if err != nil {
		return data, err
	}
	for _, t := range []tile{{State: model.StateQueued, Label: "Queued"}, {State: model.StateRunning, Label: "Running"},
		{State: model.StatePublishing, Label: "Publishing"}, {State: model.StateDone, Label: "Done"}, {State: model.StateFailed, Label: "Failed"}} {
		t.Count = counts[t.State]
		data.Tiles = append(data.Tiles, t)
	}
	if data.Backfill, err = s.svc.BackfillStatus(); err != nil {
		return data, err
	}
	jobs, err := s.svc.Store.List(queueRows)
	if err != nil {
		return data, err
	}
	for _, j := range jobs {
		if active(j) {
			data.Active = append(data.Active, j)
		}
	}
	return data, nil
}

func active(j *model.Job) bool {
	return j.State == model.StateRunning || j.State == model.StatePublishing
}

func (s *Server) dashboard(w http.ResponseWriter, _ *http.Request, sess *session) {
	data, err := s.dashboardData(sess.csrf)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, sess, "dashboard", "Dashboard", data)
}

func (s *Server) dashboardFragment(w http.ResponseWriter, _ *http.Request, sess *session) {
	data, err := s.dashboardData(sess.csrf)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.fragment(w, "dashboard", "status", data)
}

// backfillAction starts or pauses backfill, as orchestrator backfill does.
func (s *Server) backfillAction(w http.ResponseWriter, r *http.Request, sess *session) {
	var err error
	msg := flashMsg{"ok", "Backfill paused. Titles already running finish."}
	switch r.PathValue("action") {
	case "start":
		err = s.svc.SetBackfill(true)
		msg.Text = "Backfill started. It encodes within the daily budget."
	case "pause":
		err = s.svc.SetBackfill(false)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		msg = flashMsg{"error", err.Error()}
	}
	data, err := s.dashboardData(sess.csrf)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.finish(w, r, sess, "/", msg, "dashboard", "status", data)
}

// scan asks for an originals scan now instead of at the next interval.
func (s *Server) scan(w http.ResponseWriter, r *http.Request, sess *session) {
	s.svc.TriggerScan()
	page := "/"
	if r.PostForm.Get("next") == "/library" {
		page = "/library"
	}
	s.finish(w, r, sess, page, flashMsg{"ok", "Scan requested. New titles show up once it finishes."}, "dashboard", "", nil)
}
