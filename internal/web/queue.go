package web

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"jellyfinner/internal/model"
)

// queueStates are the states the queue can be filtered by, in tab order.
var queueStates = []string{model.StateQueued, model.StateRunning, model.StatePublishing,
	model.StateFailed, model.StateDone, model.StateCancelled}

// queueFilter is the queue page's query: ?state=&node=&q=&page=.
type queueFilter struct {
	State  string
	Node   string
	Search string
	Page   int
}

func parseQueueFilter(q url.Values) queueFilter {
	f := queueFilter{Node: q.Get("node"), Search: strings.TrimSpace(q.Get("q")), Page: 1}
	for _, st := range queueStates {
		if q.Get("state") == st {
			f.State = st
		}
	}
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 1 {
		f.Page = p
	}
	return f
}

// query encodes the filter, leaving out empty parts. page 0 drops the page.
// It is typed as a URL so templates keep its = and & as they are.
func (f queueFilter) query(page int) template.URL {
	v := url.Values{}
	if f.State != "" {
		v.Set("state", f.State)
	}
	if f.Node != "" {
		v.Set("node", f.Node)
	}
	if f.Search != "" {
		v.Set("q", f.Search)
	}
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	return template.URL(v.Encode())
}

// tab is one state filter above the queue.
type tab struct {
	Label   string
	Count   int
	Query   template.URL
	Current bool
}

type queueData struct {
	CSRF   string
	Filter queueFilter
	Query  template.URL // the current filter, for polling
	Tabs   []tab
	Jobs   []*model.Job
	Total  int
	First  int
	Last   int
	Prev   template.URL // query of the previous page, or ""
	Next   template.URL
}

func (s *Server) queueData(csrf string, f queueFilter) (queueData, error) {
	data := queueData{CSRF: csrf, Filter: f, Query: f.query(f.Page)}
	jobs, total, err := s.svc.Store.ListFiltered(model.JobFilter{State: f.State, Node: f.Node, Search: f.Search},
		queueRows, (f.Page-1)*queueRows)
	if err != nil {
		return data, err
	}
	data.Jobs, data.Total = jobs, total
	if len(jobs) > 0 {
		data.First = (f.Page-1)*queueRows + 1
		data.Last = data.First + len(jobs) - 1
	}
	if f.Page > 1 {
		data.Prev = f.query(f.Page - 1)
		if data.Prev == "" {
			data.Prev = "page=1"
		}
	}
	if data.Last < total {
		data.Next = f.query(f.Page + 1)
	}
	counts, err := s.svc.Store.StateCounts()
	if err != nil {
		return data, err
	}
	all := 0
	for _, n := range counts {
		all += n
	}
	data.Tabs = append(data.Tabs, tab{Label: "All", Count: all, Query: queueFilter{Node: f.Node, Search: f.Search}.query(0), Current: f.State == ""})
	for _, st := range queueStates {
		t := queueFilter{State: st, Node: f.Node, Search: f.Search}
		data.Tabs = append(data.Tabs, tab{Label: st, Count: counts[st], Query: t.query(0), Current: f.State == st})
	}
	return data, nil
}

func (s *Server) queuePage(w http.ResponseWriter, r *http.Request, sess *session) {
	data, err := s.queueData(sess.csrf, parseQueueFilter(r.URL.Query()))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, sess, "queue", "Queue", data)
}

func (s *Server) queueFragment(w http.ResponseWriter, r *http.Request, sess *session) {
	f := parseQueueFilter(r.URL.Query())
	data, err := s.queueData(sess.csrf, f)
	if err != nil {
		s.fail(w, err)
		return
	}
	// A search typed into the box changes the address, so reload and back keep it.
	if r.Header.Get("HX-Trigger-Name") == "q" {
		w.Header().Set("HX-Push-Url", s.base+"/queue"+withQuery(string(data.Query)))
	}
	s.fragment(w, "queue", "queuebody", data)
}

func withQuery(q string) string {
	if q == "" {
		return ""
	}
	return "?" + q
}

// jobData is the job page, and the fragment that refreshes it.
type jobData struct {
	CSRF string
	Job  *model.Job
	Live bool // the job may still change, so the page polls
}

func (s *Server) loadJob(w http.ResponseWriter, r *http.Request, csrf string) (jobData, bool) {
	j, err := s.svc.Store.Get(r.PathValue("id"))
	if errors.Is(err, model.ErrNotFound) {
		http.NotFound(w, r)
		return jobData{}, false
	}
	if err != nil {
		s.fail(w, err)
		return jobData{}, false
	}
	live := j.State == model.StateQueued || j.State == model.StateRunning || j.State == model.StatePublishing
	return jobData{CSRF: csrf, Job: j, Live: live}, true
}

func (s *Server) jobPage(w http.ResponseWriter, r *http.Request, sess *session) {
	if data, ok := s.loadJob(w, r, sess.csrf); ok {
		s.render(w, sess, "job", "Job "+data.Job.ID, data)
	}
}

func (s *Server) jobFragment(w http.ResponseWriter, r *http.Request, sess *session) {
	if data, ok := s.loadJob(w, r, sess.csrf); ok {
		s.fragment(w, "job", "jobview", data)
	}
}

// jobLog downloads a job's log as plain text.
func (s *Server) jobLog(w http.ResponseWriter, r *http.Request, sess *session) {
	data, ok := s.loadJob(w, r, sess.csrf)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="job-%s.log"`, safeName(data.Job.ID)))
	_, _ = w.Write([]byte(data.Job.Log))
}

// safeName keeps only characters that are harmless in a header value.
func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return -1
	}, s)
}

// logLine is one line of a job log. Bad marks lines worth reading first.
type logLine struct {
	Text string
	Bad  bool
}

func logLines(log string) []logLine {
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	out := make([]logLine, len(lines))
	for i, line := range lines {
		lower := strings.ToLower(line)
		out[i] = logLine{Text: line, Bad: strings.Contains(lower, "error") || strings.Contains(lower, "fail") ||
			strings.Contains(lower, "invalid")}
	}
	return out
}

func (s *Server) jobAction(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	var err error
	var done string
	switch r.PathValue("action") {
	case "cancel":
		err, done = s.svc.Store.Cancel(id), "Job cancelled."
	case "bump":
		err, done = s.svc.Store.Bump(id), "Moved to the front of the queue."
	case "retry":
		err, done = s.svc.Store.Retry(id), "Queued again."
	default:
		http.NotFound(w, r)
		return
	}
	msg := flashMsg{"ok", done}
	if err != nil {
		msg = flashMsg{"error", err.Error()}
	}
	if r.PostForm.Get("from") == "job" {
		data, ok := s.loadJob(w, r, sess.csrf)
		if ok {
			s.finish(w, r, sess, "/jobs/"+url.PathEscape(id), msg, "job", "jobview", data)
		}
		return
	}
	data, err := s.queueData(sess.csrf, parseQueueFilter(currentQuery(r)))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.finish(w, r, sess, "/queue", msg, "queue", "queuebody", data)
}
