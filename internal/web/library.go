package web

import (
	"net/http"
	"net/url"
	"strings"

	"jellyfinner/internal/model"
	"jellyfinner/internal/orchestrator"
)

type libraryData struct {
	CSRF   string
	Nodes  []model.Node // edge nodes, for the cache picker
	Search string
	Rows   []orchestrator.TitleRow
	Total  int
}

func (s *Server) libraryData(csrf, search string) (libraryData, error) {
	rows, total, err := s.svc.Titles(search, libraryRows)
	if err != nil {
		return libraryData{}, err
	}
	return libraryData{CSRF: csrf, Nodes: s.svc.Nodes.Edges(), Search: search, Rows: rows, Total: total}, nil
}

func (s *Server) libraryPage(w http.ResponseWriter, r *http.Request, sess *session) {
	data, err := s.libraryData(sess.csrf, strings.TrimSpace(r.URL.Query().Get("q")))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, sess, "library", "Library", data)
}

// libraryFragment answers the search box as someone types.
func (s *Server) libraryFragment(w http.ResponseWriter, r *http.Request, sess *session) {
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	data, err := s.libraryData(sess.csrf, search)
	if err != nil {
		s.fail(w, err)
		return
	}
	push := s.base + "/library"
	if search != "" {
		push += "?" + url.Values{"q": {search}}.Encode()
	}
	w.Header().Set("HX-Push-Url", push)
	s.fragment(w, "library", "librarybody", data)
}

// libraryDone redraws the title list, keeping the search the page had.
func (s *Server) libraryDone(w http.ResponseWriter, r *http.Request, sess *session, msg flashMsg) {
	data, err := s.libraryData(sess.csrf, strings.TrimSpace(currentQuery(r).Get("q")))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.finish(w, r, sess, "/library", msg, "library", "librarybody", data)
}

var nothingSelected = flashMsg{"warn", "Select at least one title."}

func (s *Server) libraryAdd(w http.ResponseWriter, r *http.Request, sess *session) {
	paths := r.PostForm["path"]
	if len(paths) == 0 {
		s.libraryDone(w, r, sess, nothingSelected)
		return
	}
	var errs []string
	done := 0
	for _, path := range paths {
		if _, err := s.svc.Add(path); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		done++
	}
	s.libraryDone(w, r, sess, summary("queued", done, errs))
}

func (s *Server) libraryCache(w http.ResponseWriter, r *http.Request, sess *session) {
	paths := r.PostForm["path"]
	if len(paths) == 0 {
		s.libraryDone(w, r, sess, nothingSelected)
		return
	}
	node := r.PostForm.Get("node")
	var errs []string
	done := 0
	for _, path := range paths {
		if err := s.svc.Pin(path, node); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		done++
	}
	s.libraryDone(w, r, sess, summary("sent to "+s.svc.NodeName(node), done, errs))
}

type confirmData struct {
	CSRF  string
	Paths []string
	Size  int64
}

// reencodeConfirm shows the cost of a re-encode before ordering it. It stays a
// full page on purpose, so pulling source data again is never one click.
func (s *Server) reencodeConfirm(w http.ResponseWriter, r *http.Request, sess *session) {
	size, known, err := s.svc.SourceSizes(r.PostForm["path"])
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(known) == 0 {
		s.flash(sess, nothingSelected.Kind, nothingSelected.Text)
		s.redirect(w, r, "/library")
		return
	}
	s.render(w, sess, "confirm", "Confirm re-encode", confirmData{CSRF: sess.csrf, Paths: known, Size: size})
}

func (s *Server) reencode(w http.ResponseWriter, r *http.Request, sess *session) {
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	if reason == "" {
		s.flash(sess, "warn", "A reason is required, so repeat problems are visible later.")
		s.redirect(w, r, "/library")
		return
	}
	opts := orchestrator.ReencodeOptions{
		Reason:      reason,
		ForceEncode: r.PostForm.Get("force_encode") != "",
		Software:    r.PostForm.Get("software") != "",
	}
	var errs []string
	done := 0
	for _, path := range r.PostForm["path"] {
		if _, err := s.svc.Reencode(path, opts); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		done++
	}
	msg := summary("re-encodes ordered", done, errs)
	s.flash(sess, msg.Kind, msg.Text)
	s.redirect(w, r, "/queue")
}
