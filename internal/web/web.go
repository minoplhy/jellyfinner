// Package web is the admin UI. It shares the orchestrator's service layer with
// the CLI, so both act the same way.
package web

import (
	"bytes"
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
	"net/url"
	"strings"
	"sync"
	"time"

	"jellyfinner/internal/buildinfo"
	"jellyfinner/internal/config"
	"jellyfinner/internal/control"
	"jellyfinner/internal/jellyfin"
	"jellyfinner/internal/orchestrator"
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

type session struct {
	user    string
	csrf    string
	expires time.Time
	flash   *flashMsg
}

// flashMsg is shown once, on the next page. Kind is ok, warn or error.
type flashMsg struct {
	Kind string
	Text string
}

type attempts struct {
	count int
	since time.Time
}

// Server is the admin UI.
type Server struct {
	svc      *orchestrator.Service
	auth     jellyfin.API // logins are checked against Jellyfin
	lastSeen func() map[string]time.Time
	// NodeReports, when set, gives each node's last self-report, by node ID.
	NodeReports func() map[string]control.NodeReport
	log         *log.Logger
	pages       map[string]*template.Template
	base        string // the path of ADMIN_URL, such as /jellyfinner-admin

	mu       sync.Mutex
	sessions map[string]*session
	logins   map[string]*attempts
}

// New parses the templates. A broken template is a build mistake, so it panics.
func New(svc *orchestrator.Service, auth jellyfin.API, lastSeen func() map[string]time.Time, logger *log.Logger) *Server {
	s := &Server{
		svc: svc, auth: auth, lastSeen: lastSeen, log: logger, base: svc.Cfg.AdminBasePath(),
		pages: map[string]*template.Template{}, sessions: map[string]*session{}, logins: map[string]*attempts{},
	}
	funcs := template.FuncMap{
		"size":      humanSize,
		"pct":       func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) },
		"ago":       func(unix int64) string { return s.ago(time.Unix(unix, 0)) },
		"agoT":      s.ago,
		"base":      func() string { return s.base },
		"splitPath": splitPath,
		// actions feeds the jobactions partial. from is "queue" or "job", the
		// page the buttons sit on, which decides what an action redraws.
		"actions": func(id, state, csrf, from string) jobActions {
			return jobActions{ID: id, State: state, CSRF: csrf, From: from}
		},
		"logLines": logLines,
		"nodeAdmin": func(id, name string, edge bool, csrf string) nodeAdmin {
			return nodeAdmin{ID: id, Name: name, Edge: edge, CSRF: csrf, Rented: !edge && strings.HasPrefix(name, "cloud-")}
		},
		// money is an estimated amount in a provider's own currency.
		"money": func(v float64, currency string) string {
			if currency == "" {
				return "unknown"
			}
			return fmt.Sprintf("%.4f %s", v, currency)
		},
		"unixT":    func(unix int64) time.Time { return time.Unix(unix, 0) },
		"clock":    func(t time.Time) string { return t.Local().Format("15:04") },
		"date":     func(t time.Time) string { return t.Local().Format("2006-01-02 15:04") },
		"datetime": func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") },
		"duration": humanDuration,
		"seconds":  func(v float64) string { return humanDuration(time.Duration(v * float64(time.Second))) },
		"version":  buildinfo.String,
		"hours":    func(v float64) string { return fmt.Sprintf("%.1f", v) },
		"gbph":     func(v float64) string { return fmt.Sprintf("%.2f", v) },
		"addBytes": func(a, b int64) int64 { return a + b },
		"month":    func(t time.Time) string { return t.Format("2006-01") },
		// pctOf is a share of a total, for meter thresholds.
		"pctOf": func(total int64, percent int64) int64 { return total / 100 * percent },
	}
	for _, page := range []string{"login", "dashboard", "queue", "job", "library", "confirm", "nodes", "nodesecret", "nodeedit", "usage"} {
		s.pages[page] = template.Must(template.New("layout.html").Funcs(funcs).
			ParseFS(assets, "templates/layout.html", "templates/"+page+".html"))
	}
	return s
}

// ago says how long ago something was, in its largest whole unit.
func (s *Server) ago(t time.Time) string {
	d := s.svc.Now().Sub(t)
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

type pathParts struct{ Dir, File string }

// splitPath separates a path's folder, kept with its slash, from its file name.
func splitPath(p string) pathParts {
	i := strings.LastIndex(p, "/")
	return pathParts{Dir: p[:i+1], File: p[i+1:]}
}

// nodeAdmin is what the nodeadmin partial needs.
type nodeAdmin struct {
	ID     string
	Name   string
	Edge   bool
	CSRF   string
	Rented bool // a rented server's node, which cannot be edited
}

// jobActions is what the jobactions partial needs.
type jobActions struct{ ID, State, CSRF, From string }

// nodeReports returns each node's self-report, or none when not wired.
func (s *Server) nodeReports() map[string]control.NodeReport {
	if s.NodeReports == nil {
		return nil
	}
	return s.NodeReports()
}

// humanDuration is "1h 02m 05s", "4m 09s" or "12s".
func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, sec := int(d/time.Hour), int(d%time.Hour/time.Minute), int(d%time.Minute/time.Second)
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %02dm %02ds", h, m, sec)
	case m > 0:
		return fmt.Sprintf("%dm %02ds", m, sec)
	}
	return fmt.Sprintf("%ds", sec)
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
	mux.HandleFunc("POST /backfill/{action}", s.protected(s.backfillAction))
	mux.HandleFunc("POST /scan", s.protected(s.scan))
	mux.HandleFunc("GET /queue", s.protected(s.queuePage))
	mux.HandleFunc("GET /fragments/queue", s.protected(s.queueFragment))
	mux.HandleFunc("GET /jobs/{id}", s.protected(s.jobPage))
	mux.HandleFunc("GET /jobs/{id}/log", s.protected(s.jobLog))
	mux.HandleFunc("GET /fragments/jobs/{id}", s.protected(s.jobFragment))
	mux.HandleFunc("POST /jobs/{id}/{action}", s.protected(s.jobAction))
	mux.HandleFunc("GET /library", s.protected(s.libraryPage))
	mux.HandleFunc("GET /fragments/library", s.protected(s.libraryFragment))
	mux.HandleFunc("POST /library/add", s.protected(s.libraryAdd))
	mux.HandleFunc("POST /library/cache", s.protected(s.libraryCache))
	mux.HandleFunc("POST /library/reencode", s.protected(s.reencodeConfirm))
	mux.HandleFunc("POST /library/reencode/confirm", s.protected(s.reencode))
	mux.HandleFunc("GET /nodes", s.protected(s.nodesPage))
	mux.HandleFunc("GET /fragments/nodes/{part}", s.protected(s.nodesFragment))
	mux.HandleFunc("POST /nodes/add", s.protected(s.nodeAdd))
	mux.HandleFunc("POST /nodes/{node}/remove", s.protected(s.nodeRemove))
	mux.HandleFunc("POST /nodes/{node}/rotate", s.protected(s.nodeRotate))
	mux.HandleFunc("POST /nodes/{node}/delete", s.protected(s.nodeDelete))
	mux.HandleFunc("GET /nodes/{node}/edit", s.protected(s.nodeEditPage))
	mux.HandleFunc("POST /nodes/{node}/edit", s.protected(s.nodeEdit))
	mux.HandleFunc("POST /nodes/{node}/viewers/{action}", s.protected(s.nodeViewer))
	mux.HandleFunc("POST /nodes/{node}/{action}", s.protected(s.nodePause))
	mux.HandleFunc("GET /usage", s.protected(s.usagePage))
	mux.HandleFunc("GET /fragments/usage", s.protected(s.usageFragment))
	mux.HandleFunc("POST /cloud/{action}", s.protected(s.cloudAction))
	mux.HandleFunc("POST /cloud/servers/{name}/drain", s.protected(s.cloudDrain))
	return headers(config.StripPrefix(s.base, mux))
}

// redirect sends the browser to a page of the UI, under its base path.
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, page string) {
	http.Redirect(w, r, s.base+page, http.StatusSeeOther)
}

// cookiePath limits the session to the UI's own path.
func (s *Server) cookiePath() string {
	if s.base == "" {
		return "/"
	}
	return s.base
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

func (s *Server) now() time.Time { return s.svc.Now() }

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
			s.redirect(w, r, "/login")
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
	Page    string
	User    string
	CSRF    string
	Flash   *flashMsg
	Mode    string
	Content any
}

func (s *Server) render(w http.ResponseWriter, sess *session, page, title string, content any) {
	data := pageData{Title: title, Page: page, Mode: string(s.svc.Cfg.Mode), Content: content}
	if sess != nil {
		s.mu.Lock()
		data.User, data.CSRF, data.Flash = sess.user, sess.csrf, sess.flash
		sess.flash = nil
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

func (s *Server) flash(sess *session, kind, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.flash = &flashMsg{Kind: kind, Text: message}
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.currentSession(r) != nil {
		s.redirect(w, r, "/")
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
		Name: cookieName, Value: id, Path: s.cookiePath(), HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: s.svc.Cfg.AdminHTTPS() || r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge: int(sessionLife.Seconds()),
	})
	s.redirect(w, r, "/")
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, _ *session) {
	if cookie, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, cookie.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: s.cookiePath(), MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	s.redirect(w, r, "/login")
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// finish ends a request that changed something. A plain form post is
// redirected to page, which shows the message. An htmx request instead gets
// the named fragment from set, the part of the page that changed, followed by
// the message as a toast. name may be empty when nothing on the page changed.
func (s *Server) finish(w http.ResponseWriter, r *http.Request, sess *session, page string, msg flashMsg, set, name string, data any) {
	if !isHTMX(r) {
		if msg.Text != "" {
			s.flash(sess, msg.Kind, msg.Text)
		}
		s.redirect(w, r, page)
		return
	}
	var buf bytes.Buffer
	if name != "" {
		if err := s.pages[set].ExecuteTemplate(&buf, name, data); err != nil {
			s.fail(w, err)
			return
		}
	}
	if msg.Text != "" {
		if err := s.pages[set].ExecuteTemplate(&buf, "toast", msg); err != nil {
			s.fail(w, err)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// currentQuery is the query of the page an htmx request came from, so a
// redrawn fragment keeps that page's filters.
func currentQuery(r *http.Request) url.Values {
	u, err := url.Parse(r.Header.Get("HX-Current-URL"))
	if err != nil {
		return url.Values{}
	}
	return u.Query()
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Printf("web: %v", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// summary reports a batch action, as a warning when anything was refused.
func summary(verb string, done int, errs []string) flashMsg {
	message := fmt.Sprintf("%d %s.", done, verb)
	if len(errs) > 0 {
		return flashMsg{"warn", message + " Refused: " + strings.Join(errs, "; ")}
	}
	return flashMsg{"ok", message}
}
