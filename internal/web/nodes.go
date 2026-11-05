package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"jellyfinner/internal/model"
	"jellyfinner/internal/orchestrator"
)

// workerView is an encoding node on the Nodes page.
type workerView struct {
	Name   string
	Paused bool
	State  nodeState
	Seen   time.Time
	Active []*model.Job
	Failed int
}

// edgeView is an edge node on the Nodes page.
type edgeView struct {
	orchestrator.NodeRow
	State    nodeState
	DiskUsed int64
}

type nodesData struct {
	CSRF    string
	Workers []workerView
	Edges   []edgeView
	// Cloud is filled only for the full page and its own fragment, because
	// it may ask the providers for prices.
	Cloud *cloudView
}

// cloudView is the Cloud workers section. Every amount is an estimate,
// excluding VAT, at the prices quoted when each server started.
type cloudView struct {
	CSRF     string
	Paused   bool
	Order    []string
	Disabled map[string]string
	Servers  []orchestrator.ServerView
	Usage    orchestrator.Usage
	HoursCap float64
	Billing  []orchestrator.ProviderBilling
	Estimate orchestrator.Estimate
	PricedAt time.Time
	Measured bool
	Err      string
}

func (s *Server) cloudData(r *http.Request, csrf string) *cloudView {
	c := s.svc.Cloud
	if c == nil {
		return nil
	}
	v := &cloudView{CSRF: csrf, HoursCap: c.HoursCap(), Billing: c.Billing(), Disabled: c.Disabled()}
	for _, p := range c.Providers() {
		v.Order = append(v.Order, p.Name())
	}
	var errs []string
	var err error
	if v.Paused, err = c.Paused(); err != nil {
		errs = append(errs, err.Error())
	}
	if v.Servers, err = c.Servers(); err != nil {
		errs = append(errs, err.Error())
	}
	if v.Usage, err = c.Usage(r.Context()); err != nil {
		errs = append(errs, err.Error())
	}
	if v.Estimate, err = c.Estimate(r.Context()); err != nil {
		errs = append(errs, err.Error())
	}
	for _, row := range v.Estimate.Rows {
		v.Measured = v.Measured || row.Jobs > 0
		if v.PricedAt.IsZero() || (!row.PricedAt.IsZero() && row.PricedAt.Before(v.PricedAt)) {
			v.PricedAt = row.PricedAt
		}
	}
	v.Err = strings.Join(errs, "; ")
	return v
}

func (s *Server) nodesData(csrf string) (nodesData, error) {
	data := nodesData{CSRF: csrf}
	jobs, err := s.svc.Store.List(queueRows)
	if err != nil {
		return data, err
	}
	failed, err := s.svc.Store.FailedByNode(s.now().Add(-24 * time.Hour))
	if err != nil {
		return data, err
	}
	seen := s.lastSeen()
	for _, n := range s.svc.Nodes.All() {
		if n.Edge() {
			continue
		}
		v := workerView{Name: n.Name, Paused: n.Paused, Seen: seen[n.Name], Failed: failed[n.Name]}
		v.State = s.workerState(v.Seen, n.Paused)
		for _, j := range jobs {
			if active(j) && j.Node == n.Name {
				v.Active = append(v.Active, j)
			}
		}
		data.Workers = append(data.Workers, v)
	}
	rows, err := s.svc.NodeRows()
	if err != nil {
		return data, err
	}
	for _, row := range rows {
		data.Edges = append(data.Edges, edgeView{NodeRow: row, State: s.edgeState(row.Status),
			DiskUsed: row.Status.DiskTotal - row.Status.DiskFree})
	}
	return data, nil
}

func (s *Server) nodesPage(w http.ResponseWriter, r *http.Request, sess *session) {
	data, err := s.nodesData(sess.csrf)
	if err != nil {
		s.fail(w, err)
		return
	}
	data.Cloud = s.cloudData(r, sess.csrf)
	s.render(w, sess, "nodes", "Nodes", data)
}

// nodesFragment refreshes one part of the Nodes page: workers or edges.
func (s *Server) nodesFragment(w http.ResponseWriter, r *http.Request, sess *session) {
	part := r.PathValue("part")
	if part != "workers" && part != "edges" && part != "cloud" {
		http.NotFound(w, r)
		return
	}
	data, err := s.nodesData(sess.csrf)
	if err != nil {
		s.fail(w, err)
		return
	}
	if part == "cloud" {
		data.Cloud = s.cloudData(r, sess.csrf)
	}
	s.fragment(w, "nodes", part, data)
}

// nodeRemove deletes a cached title from an edge node.
func (s *Server) nodeRemove(w http.ResponseWriter, r *http.Request, sess *session) {
	msg := flashMsg{"ok", "Removing it from " + r.PathValue("node") + "."}
	if err := s.svc.UnpinOutput(r.PostForm.Get("output"), r.PathValue("node"), r.PostForm.Get("force") != ""); err != nil {
		msg = flashMsg{"error", err.Error()}
	}
	data, err := s.nodesData(sess.csrf)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.finish(w, r, sess, "/nodes", msg, "nodes", "edges", data)
}

// secretData is the one-time page with what a node puts in its compose file.
type secretData struct {
	Name       string
	Edge       bool
	Rotated    bool
	ControlURL string
	ControlSet bool
	Token      string
	SigningKey string
}

func (s *Server) secretPage(w http.ResponseWriter, sess *session, d secretData) {
	d.ControlURL, d.ControlSet = s.svc.Cfg.ControlURL, s.svc.Cfg.ControlURL != ""
	if !d.ControlSet {
		d.ControlURL = orchestrator.ControlURLPlaceholder
	}
	title := "Node added"
	if d.Rotated {
		title = "New token"
	}
	s.render(w, sess, "nodesecret", title, d)
}

// nodeAdd registers a node, as orchestrator node add does, and shows its
// secrets this once.
func (s *Server) nodeAdd(w http.ResponseWriter, r *http.Request, sess *session) {
	spec := orchestrator.NodeSpec{
		Name:  strings.TrimSpace(r.PostForm.Get("name")),
		Edge:  r.PostForm.Get("kind") == "edge",
		Allow: strings.TrimSpace(r.PostForm.Get("allow")),
	}
	if spec.Edge {
		spec.URL = strings.TrimSpace(r.PostForm.Get("url"))
		quota, err := strconv.ParseInt(strings.TrimSpace(r.PostForm.Get("quota_gb")), 10, 64)
		if err != nil {
			s.flash(sess, "error", "The quota must be a whole number of GB.")
			s.redirect(w, r, "/nodes")
			return
		}
		spec.QuotaGB = quota
	}
	added, err := s.svc.AddNode(spec)
	if err != nil {
		s.flash(sess, "error", "Could not add "+spec.Name+": "+err.Error())
		s.redirect(w, r, "/nodes")
		return
	}
	s.log.Printf("web: %s added node %s", sess.user, spec.Name)
	s.secretPage(w, sess, secretData{Name: spec.Name, Edge: spec.Edge, Token: added.Token, SigningKey: added.SigningKey})
}

func (s *Server) nodeRotate(w http.ResponseWriter, r *http.Request, sess *session) {
	name := r.PathValue("node")
	token, err := s.svc.RotateNodeToken(name)
	if err != nil {
		s.flash(sess, "error", err.Error())
		s.redirect(w, r, "/nodes")
		return
	}
	s.log.Printf("web: %s rotated the token of node %s", sess.user, name)
	s.secretPage(w, sess, secretData{Name: name, Rotated: true, Token: token})
}

// cloudAction pauses or resumes new cloud servers, as orchestrator cloud
// pause does.
func (s *Server) cloudAction(w http.ResponseWriter, r *http.Request, sess *session) {
	c := s.svc.Cloud
	action := r.PathValue("action")
	if c == nil || (action != "pause" && action != "resume") {
		http.NotFound(w, r)
		return
	}
	msg := flashMsg{"ok", "New cloud servers start again when the queue needs them."}
	if action == "pause" {
		msg = flashMsg{"ok", "Paused: no new cloud server starts. Running ones finish and are deleted as they go idle."}
	}
	if err := c.SetPaused(action == "pause"); err != nil {
		msg = flashMsg{"error", err.Error()}
	} else {
		s.log.Printf("web: %s %sd cloud workers", sess.user, action)
	}
	s.finishCloud(w, r, sess, msg)
}

// cloudDrain drains one rented server, which is deleted once its job ends.
func (s *Server) cloudDrain(w http.ResponseWriter, r *http.Request, sess *session) {
	c := s.svc.Cloud
	if c == nil {
		http.NotFound(w, r)
		return
	}
	name := r.PathValue("name")
	msg := flashMsg{"ok", "Draining " + name + ": it finishes its job, then is deleted."}
	if err := c.Drain(name); err != nil {
		msg = flashMsg{"error", err.Error()}
	} else {
		s.log.Printf("web: %s drained cloud server %s", sess.user, name)
	}
	s.finishCloud(w, r, sess, msg)
}

func (s *Server) finishCloud(w http.ResponseWriter, r *http.Request, sess *session, msg flashMsg) {
	data, err := s.nodesData(sess.csrf)
	if err != nil {
		s.fail(w, err)
		return
	}
	data.Cloud = s.cloudData(r, sess.csrf)
	s.finish(w, r, sess, "/nodes", msg, "nodes", "cloud", data)
}

// nodePause pauses or resumes a worker, as orchestrator node pause does.
func (s *Server) nodePause(w http.ResponseWriter, r *http.Request, sess *session) {
	name, action := r.PathValue("node"), r.PathValue("action")
	var err error
	var msg flashMsg
	switch action {
	case "pause":
		err = s.svc.PauseNode(name)
		msg = flashMsg{"ok", "Paused " + name + ". A job it holds runs to the end, then it takes no more."}
	case "resume":
		err = s.svc.ResumeNode(name)
		msg = flashMsg{"ok", "Resumed " + name + "."}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		msg = flashMsg{"error", err.Error()}
	} else {
		s.log.Printf("web: %s %sd node %s", sess.user, action, name)
	}
	data, derr := s.nodesData(sess.csrf)
	if derr != nil {
		s.fail(w, derr)
		return
	}
	s.finish(w, r, sess, "/nodes", msg, "nodes", "workers", data)
}

// nodeDelete revokes a node, as orchestrator node remove does.
func (s *Server) nodeDelete(w http.ResponseWriter, r *http.Request, sess *session) {
	name := r.PathValue("node")
	if err := s.svc.Nodes.Remove(name); err != nil {
		s.flash(sess, "error", err.Error())
	} else {
		s.log.Printf("web: %s removed node %s", sess.user, name)
		s.flash(sess, "ok", "Removed "+name+". Its token no longer works.")
	}
	s.redirect(w, r, "/nodes")
}
