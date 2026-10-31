// Package web is the admin UI. It shares the orchestrator's service layer with
// the CLI, so both act the same way.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"jellyfinner/internal/jellyfin"
	"jellyfinner/internal/orchestrator"
	"jellyfinner/internal/queue"
)

//go:embed templates/*.html static/*
var assets embed.FS

const (
	cookieName    = "jellyfinner_session"
	sessionLife   = 12 * time.Hour
	loginLimit    = 10
	loginWindow   = time.Minute
	maxForm       = 1 << 20
	libraryRows   = 200
	queueRows     = 100
	contentPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
		"form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
)

// Authenticator checks logins against Jellyfin.
type Authenticator interface {
	Authenticate(ctx context.Context, username, password string) (admin bool, token string, err error)
	EndSession(ctx context.Context, token string) error
}

type session struct {
	user    string
	csrf    string
	expires time.Time
	flash   string
}

type attempts struct {
	count int
	since time.Time
}

// Server is the admin UI.
type Server struct {
	svc      *orchestrator.Service
	auth     Authenticator
	lastSeen func() map[string]time.Time
	log      *log.Logger
	pages    map[string]*template.Template

	mu       sync.Mutex
	sessions map[string]*session
	logins   map[string]*attempts
}

// New parses the templates. A broken template is a build mistake, so it panics.
func New(svc *orchestrator.Service, auth Authenticator, lastSeen func() map[string]time.Time, logger *log.Logger) *Server {
	s := &Server{
		svc: svc, auth: auth, lastSeen: lastSeen, log: logger,
		pages: map[string]*template.Template{}, sessions: map[string]*session{}, logins: map[string]*attempts{},
	}
	funcs := template.FuncMap{
		"size": humanSize,
		"pct":  func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) },
		"ago":  func(unix int64) string { return s.ago(time.Unix(unix, 0)) },
		"agoT": s.ago,
	}
	for _, page := range []string{"login", "dashboard", "queue", "job", "library", "confirm", "nodes"} {
		s.pages[page] = template.Must(template.New("layout.html").Funcs(funcs).
			ParseFS(assets, "templates/layout.html", "templates/"+page+".html"))
	}
	return s
}

func (s *Server) ago(t time.Time) string {
	d := s.svc.Store.Now().Sub(t).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String() + " ago"
}

func humanSize(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.0f MB", float64(n)/1e6)
	}
	return fmt.Sprintf("%d KB", n/1000)
}

// Handler returns every route.
func (s *Server) Handler() http.Handler {
	static, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.protected(s.logout))
	mux.HandleFunc("GET /{$}", s.protected(s.dashboard))
	mux.HandleFunc("GET /fragments/dashboard", s.protected(s.dashboardFragment))
	mux.HandleFunc("GET /queue", s.protected(s.queuePage))
	mux.HandleFunc("GET /fragments/queue", s.protected(s.queueFragment))
	mux.HandleFunc("GET /jobs/{id}", s.protected(s.jobPage))
	mux.HandleFunc("POST /jobs/{id}/{action}", s.protected(s.jobAction))
	mux.HandleFunc("GET /library", s.protected(s.libraryPage))
	mux.HandleFunc("POST /library/add", s.protected(s.libraryAdd))
	mux.HandleFunc("POST /library/cache", s.protected(s.libraryCache))
	mux.HandleFunc("GET /nodes", s.protected(s.nodesPage))
	mux.HandleFunc("POST /nodes/{node}/remove", s.protected(s.nodeRemove))
	mux.HandleFunc("POST /library/reencode", s.protected(s.reencodeConfirm))
	mux.HandleFunc("POST /library/reencode/confirm", s.protected(s.reencode))
	return headers(mux)
}

func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) now() time.Time { return s.svc.Store.Now() }

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *Server) currentSession(r *http.Request) *session {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[cookie.Value]
	if sess == nil {
		return nil
	}
	if s.now().After(sess.expires) {
		delete(s.sessions, cookie.Value)
		return nil
	}
	return sess
}

// protected requires a login, and a CSRF token on anything that changes state.
func (s *Server) protected(next func(http.ResponseWriter, *http.Request, *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.currentSession(r)
		if sess == nil {
			if r.Header.Get("HX-Request") != "" || r.Method != http.MethodGet {
				http.Error(w, "login required", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method != http.MethodGet {
			r.Body = http.MaxBytesReader(w, r.Body, maxForm)
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(sess.csrf)) != 1 {
				http.Error(w, "invalid form token", http.StatusForbidden)
				return
			}
		}
		next(w, r, sess)
	}
}

func (s *Server) clientAddr(r *http.Request) string {
	addr, ok := s.svc.Cfg.ClientAddr(r)
	if !ok {
		return r.RemoteAddr
	}
	return addr.String()
}

func (s *Server) loginBlocked(addr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.logins[addr]
	if a == nil {
		return false
	}
	if s.now().Sub(a.since) > loginWindow {
		delete(s.logins, addr)
		return false
	}
	return a.count >= loginLimit
}

func (s *Server) loginFailed(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.logins[addr]; a != nil {
		a.count++
		return
	}
	s.logins[addr] = &attempts{count: 1, since: s.now()}
}

type pageData struct {
	Title   string
	User    string
	CSRF    string
	Flash   string
	Mode    string
	Content any
}

func (s *Server) render(w http.ResponseWriter, sess *session, page, title string, content any) {
	data := pageData{Title: title, Mode: string(s.svc.Cfg.Mode), Content: content}
	if sess != nil {
		s.mu.Lock()
		data.User, data.CSRF, data.Flash = sess.user, sess.csrf, sess.flash
		sess.flash = ""
		s.mu.Unlock()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[page].Execute(w, data); err != nil {
		s.log.Printf("render %s: %v", page, err)
	}
}

func (s *Server) fragment(w http.ResponseWriter, page, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[page].ExecuteTemplate(w, name, data); err != nil {
		s.log.Printf("render %s: %v", name, err)
	}
}

func (s *Server) flash(sess *session, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.flash = message
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.currentSession(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, nil, "login", "Sign in", "")
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	addr := s.clientAddr(r)
	if s.loginBlocked(addr) {
		http.Error(w, "too many attempts, try again in a minute", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	username := r.PostForm.Get("username")
	admin, token, err := s.auth.Authenticate(r.Context(), username, r.PostForm.Get("password"))
	if token != "" {
		// Only the yes or no is needed, so Jellyfin's session is closed at once.
		if err := s.auth.EndSession(context.WithoutCancel(r.Context()), token); err != nil {
			s.log.Printf("end jellyfin session: %v", err)
		}
	}
	if err != nil && !errors.Is(err, jellyfin.ErrBadLogin) {
		s.log.Printf("login: %v", err)
		w.WriteHeader(http.StatusBadGateway)
		s.render(w, nil, "login", "Sign in", "Could not reach Jellyfin.")
		return
	}
	if err != nil || !admin {
		// One message for both cases, so the page does not confirm an account.
		s.loginFailed(addr)
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, nil, "login", "Sign in", "Wrong login, or not a Jellyfin administrator.")
		return
	}
	id, err := randomToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	csrf, err := randomToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	for key, sess := range s.sessions {
		if s.now().After(sess.expires) {
			delete(s.sessions, key)
		}
	}
	s.sessions[id] = &session{user: username, csrf: csrf, expires: s.now().Add(sessionLife)}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge: int(sessionLife.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, _ *session) {
	if cookie, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, cookie.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type nodeRow struct {
	Name   string
	Seen   time.Time
	Online bool
}

type dashboardData struct {
	Nodes    []nodeRow
	Counts   map[string]int
	Active   []*queue.Job
	Backfill orchestrator.BackfillStatus
	Budget   int64
}

func (s *Server) dashboardData() (dashboardData, error) {
	data := dashboardData{Budget: s.svc.Cfg.BackfillDailyBytes}
	seen := s.lastSeen()
	for _, node := range s.svc.Cfg.Nodes {
		row := nodeRow{Name: node.Name, Seen: seen[node.Name]}
		row.Online = !row.Seen.IsZero() && s.now().Sub(row.Seen) < 2*s.svc.Cfg.LeaseFor
		data.Nodes = append(data.Nodes, row)
	}
	var err error
	if data.Counts, err = s.svc.Store.StateCounts(); err != nil {
		return data, err
	}
	if data.Backfill, err = s.svc.BackfillStatus(); err != nil {
		return data, err
	}
	jobs, err := s.svc.Store.List(queueRows)
	if err != nil {
		return data, err
	}
	for _, j := range jobs {
		if j.State == queue.StateRunning || j.State == queue.StatePublishing {
			data.Active = append(data.Active, j)
		}
	}
	return data, nil
}

func (s *Server) dashboard(w http.ResponseWriter, _ *http.Request, sess *session) {
	data, err := s.dashboardData()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, sess, "dashboard", "Dashboard", data)
}

func (s *Server) dashboardFragment(w http.ResponseWriter, _ *http.Request, _ *session) {
	data, err := s.dashboardData()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.fragment(w, "dashboard", "status", data)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Printf("web: %v", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

type queueData struct {
	CSRF string
	Jobs []*queue.Job
}

func (s *Server) queuePage(w http.ResponseWriter, _ *http.Request, sess *session) {
	jobs, err := s.svc.Store.List(queueRows)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, sess, "queue", "Queue", queueData{CSRF: sess.csrf, Jobs: jobs})
}

func (s *Server) queueFragment(w http.ResponseWriter, _ *http.Request, sess *session) {
	jobs, err := s.svc.Store.List(queueRows)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.fragment(w, "queue", "rows", queueData{CSRF: sess.csrf, Jobs: jobs})
}

func (s *Server) jobPage(w http.ResponseWriter, r *http.Request, sess *session) {
	j, err := s.svc.Store.Get(r.PathValue("id"))
	if errors.Is(err, queue.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, sess, "job", "Job "+j.ID, j)
}

func (s *Server) jobAction(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	var err error
	switch r.PathValue("action") {
	case "cancel":
		err = s.svc.Store.Cancel(id)
	case "bump":
		err = s.svc.Store.Bump(id)
	case "retry":
		err = s.svc.Store.Retry(id)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.flash(sess, err.Error())
	}
	http.Redirect(w, r, "/queue", http.StatusSeeOther)
}

type libraryData struct {
	CSRF   string
	Nodes  []string
	Search string
	Rows   []orchestrator.TitleRow
	Total  int
}

func (s *Server) libraryPage(w http.ResponseWriter, r *http.Request, sess *session) {
	search := r.URL.Query().Get("q")
	rows, total, err := s.svc.Titles(search, libraryRows)
	if err != nil {
		s.fail(w, err)
		return
	}
	var nodes []string
	for _, n := range s.svc.Cfg.EdgeNodes {
		nodes = append(nodes, n.Name)
	}
	s.render(w, sess, "library", "Library", libraryData{CSRF: sess.csrf, Nodes: nodes, Search: search, Rows: rows, Total: total})
}

func (s *Server) libraryCache(w http.ResponseWriter, r *http.Request, sess *session) {
	node := r.PostForm.Get("node")
	var errs []string
	done := 0
	for _, path := range r.PostForm["path"] {
		if err := s.svc.Pin(path, node); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		done++
	}
	s.flash(sess, summary("sent to "+node, done, errs))
	http.Redirect(w, r, "/library", http.StatusSeeOther)
}

type nodesData struct {
	CSRF  string
	Nodes []orchestrator.NodeRow
}

func (s *Server) nodesPage(w http.ResponseWriter, _ *http.Request, sess *session) {
	rows, err := s.svc.NodeRows()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, sess, "nodes", "Nodes", nodesData{CSRF: sess.csrf, Nodes: rows})
}

func (s *Server) nodeRemove(w http.ResponseWriter, r *http.Request, sess *session) {
	err := s.svc.UnpinOutput(r.PostForm.Get("output"), r.PathValue("node"), r.PostForm.Get("force") != "")
	if err != nil {
		s.flash(sess, err.Error())
	}
	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

func (s *Server) libraryAdd(w http.ResponseWriter, r *http.Request, sess *session) {
	var errs []string
	done := 0
	for _, path := range r.PostForm["path"] {
		if _, err := s.svc.Add(path); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		done++
	}
	s.flash(sess, summary("queued", done, errs))
	http.Redirect(w, r, "/library", http.StatusSeeOther)
}

func summary(verb string, done int, errs []string) string {
	message := fmt.Sprintf("%d %s.", done, verb)
	if len(errs) > 0 {
		message += " Refused: " + strings.Join(errs, "; ")
	}
	return message
}

type confirmData struct {
	CSRF  string
	Paths []string
	Size  int64
}

func (s *Server) reencodeConfirm(w http.ResponseWriter, r *http.Request, sess *session) {
	size, known, err := s.svc.SourceSizes(r.PostForm["path"])
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(known) == 0 {
		s.flash(sess, "Select at least one title.")
		http.Redirect(w, r, "/library", http.StatusSeeOther)
		return
	}
	s.render(w, sess, "confirm", "Confirm re-encode", confirmData{CSRF: sess.csrf, Paths: known, Size: size})
}

func (s *Server) reencode(w http.ResponseWriter, r *http.Request, sess *session) {
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	if reason == "" {
		s.flash(sess, "A reason is required, so repeat problems are visible later.")
		http.Redirect(w, r, "/library", http.StatusSeeOther)
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
	s.flash(sess, summary("re-encodes ordered", done, errs))
	http.Redirect(w, r, "/queue", http.StatusSeeOther)
}
