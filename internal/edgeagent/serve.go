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

// ServeHTTP answers under any path prefix, such as /edge1, so a reverse proxy
// may strip it or not. The prefix grants nothing: the signature covers the
// canonical /e/<hash>.mp4, and the file is chosen by a checked hash alone.
func (s *FileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" || strings.HasSuffix(r.URL.Path, "/health") {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	var hash string
	i := strings.LastIndex(r.URL.Path, "/e/")
	ok := i >= 0
	if ok {
		hash, ok = strings.CutSuffix(r.URL.Path[i+len("/e/"):], ".mp4")
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
	err := edge.CheckSignedURL(s.Key, edge.FilePath(hash), query.Get("md5"), query.Get("expires"), now())
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
