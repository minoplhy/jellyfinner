package edgeagent

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"jellyfinner/internal/edge"
)

// FileServer answers viewers on a node. It runs apart from the pull loop, and
// holds only the signing key. Any reverse proxy can sit in front of it for TLS.
type FileServer struct {
	Dir string
	Key string
	Now func() time.Time
}

func (s *FileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	hash, ok := strings.CutPrefix(r.URL.Path, "/e/")
	if ok {
		hash, ok = strings.CutSuffix(hash, ".mp4")
	}
	if !ok || !edge.ValidHash(hash) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	query := r.URL.Query()
	err := edge.CheckSignedURL(s.Key, r.URL.Path, query.Get("md5"), query.Get("expires"), now())
	if errors.Is(err, edge.ErrExpired) {
		http.Error(w, "expired", http.StatusGone)
		return
	}
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	f, err := os.Open(filepath.Join(s.Dir, hash+".mp4"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "private, no-store")
	// Web players are on the Jellyfin origin.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	http.ServeContent(w, r, "", info.ModTime(), f)
}
