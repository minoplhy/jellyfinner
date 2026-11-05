package api

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"jellyfinner/internal/control"
	"jellyfinner/internal/model"
)

const (
	// maxUpload stops a broken or stolen worker filling the disk.
	maxUpload        = 256 << 30
	transferDeadline = 48 * time.Hour
)

// leasedJob returns the job only while the calling node holds its lease, so a
// worker can reach the source and output of its current job and nothing else.
func (s *Server) leasedJob(w http.ResponseWriter, r *http.Request) (*model.Job, bool) {
	j, err := s.store.Get(r.PathValue("id"))
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		s.fail(w, err)
		return nil, false
	}
	lease := r.Header.Get(control.LeaseHeader)
	if err != nil || j.State != model.StateRunning || j.Node != nodeID(r) || lease == "" ||
		subtle.ConstantTimeCompare([]byte(lease), []byte(j.LeaseID)) != 1 {
		http.Error(w, "lease lost", http.StatusConflict)
		return nil, false
	}
	return j, true
}

func (s *Server) extendTransfer(w http.ResponseWriter) {
	// The listener's timeouts suit small calls, not a whole title.
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(transferDeadline)
	if err := rc.SetReadDeadline(deadline); err != nil {
		s.log.Printf("transfer deadline: %v", err)
	}
	if err := rc.SetWriteDeadline(deadline); err != nil {
		s.log.Printf("transfer deadline: %v", err)
	}
}

// jobSource serves the source with Range support. The path comes from the
// database, never the request.
func (s *Server) jobSource(w http.ResponseWriter, r *http.Request) {
	j, ok := s.leasedJob(w, r)
	if !ok {
		return
	}
	f, err := os.Open(filepath.Join(s.cfg.OriginalsDir, filepath.FromSlash(j.Path)))
	if err != nil {
		http.Error(w, "gone", http.StatusGone)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		s.fail(w, err)
		return
	}
	// Chunks arrive over many requests, so a replaced file would mix two versions.
	if info.Size() != j.SourceSize || info.ModTime().UnixNano() != j.SourceMtime {
		http.Error(w, "source changed", http.StatusGone)
		return
	}
	s.extendTransfer(w)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// uploadPath is the one place a job's files may go. Names follow the same rule
// as the manifest check in the publish step.
func (s *Server) uploadPath(j *model.Job, name string) (string, bool) {
	if name == "" || name == "." || name == ".." || name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		return "", false
	}
	return filepath.Join(s.cfg.IncomingDir, j.ID, name), true
}

// jobFileOffset reports how much of a file has arrived, so an upload can
// resume.
func (s *Server) jobFileOffset(w http.ResponseWriter, r *http.Request) {
	j, ok := s.leasedJob(w, r)
	if !ok {
		return
	}
	target, ok := s.uploadPath(j, r.PathValue("name"))
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var size int64
	if info, err := os.Stat(target); err == nil {
		size = info.Size()
	}
	w.Header().Set(control.OffsetHeader, strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusNoContent)
}

// jobFileUpload writes into the job's own folder. Offset 0 starts the file
// again, which clears what an earlier attempt left. An incomplete file does no
// harm, because the manifest is pushed last and its checksums are verified
// before publishing.
func (s *Server) jobFileUpload(w http.ResponseWriter, r *http.Request) {
	j, ok := s.leasedJob(w, r)
	if !ok {
		return
	}
	target, ok := s.uploadPath(j, r.PathValue("name"))
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	offset, err := strconv.ParseInt(r.Header.Get(control.OffsetHeader), 10, 64)
	if err != nil || offset < 0 {
		http.Error(w, "bad offset", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		s.fail(w, err)
		return
	}
	flags := os.O_CREATE | os.O_WRONLY
	if offset == 0 {
		flags |= os.O_TRUNC
	}
	// Jellyfin reads these once published, often as another user.
	f, err := os.OpenFile(target, flags, 0o644)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		s.fail(w, err)
		return
	}
	if info.Size() != offset {
		w.Header().Set(control.OffsetHeader, strconv.FormatInt(info.Size(), 10))
		http.Error(w, "wrong offset", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		s.fail(w, err)
		return
	}
	s.extendTransfer(w)
	written, err := io.Copy(f, http.MaxBytesReader(w, r.Body, maxUpload-offset))
	w.Header().Set(control.OffsetHeader, strconv.FormatInt(offset+written, 10))
	if err != nil {
		http.Error(w, fmt.Sprintf("upload stopped at %d", offset+written), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
