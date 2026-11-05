package orchestrator

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"jellyfinner/internal/model"
)

const maxWebhookBody = 64 << 10

// PlaybackEvent is the JSON the Jellyfin Webhook plugin sends. Its template is
// set by the admin, see deploy/jellyfin-webhook.json.
type PlaybackEvent struct {
	NotificationType string `json:"NotificationType"`
	ItemID           string `json:"ItemId"`
	UserID           string `json:"UserId"`
	Username         string `json:"NotificationUsername"`
	SessionID        string `json:"SessionId"`
}

// WebhookHandler receives playback events. It only exists in manual mode.
func (s *Service) WebhookHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/jellyfin", func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Webhook-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.Cfg.WebhookToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
		var event PlaybackEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := s.HandlePlayback(r.Context(), event); err != nil {
			s.Log.Printf("webhook: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// HandlePlayback queues the title if the viewer pressed play on a stub.
func (s *Service) HandlePlayback(ctx context.Context, event PlaybackEvent) error {
	if event.NotificationType != "PlaybackStart" || event.ItemID == "" {
		return nil
	}
	itemPath, err := s.Library.ItemPath(ctx, event.ItemID)
	if err != nil {
		return fmt.Errorf("item %s: %w", event.ItemID, err)
	}
	output, ok := s.libraryRelative(itemPath)
	if !ok {
		return nil
	}
	isStub, err := s.Stub.Is(filepath.Join(s.Cfg.LibraryDir, filepath.FromSlash(output)))
	if err != nil {
		return err
	}
	if !isStub {
		return nil
	}

	src, err := s.SourceByOutput(output)
	if errors.Is(err, model.ErrNotFound) {
		s.Log.Printf("stub %s has no tracked source", output)
		return nil
	}
	if err != nil {
		return err
	}
	if event.UserID != "" {
		req := model.Request{Path: src.Path, UserID: event.UserID, Username: event.Username, ItemID: event.ItemID}
		if err := s.Store.AddRequest(req); err != nil {
			return err
		}
	}

	j, err := s.ensureQueued(src)
	if err != nil {
		return err
	}
	message := "Queued. You will be notified when it is ready."
	if j.State == model.StateFailed {
		message = "This title could not be prepared. The admin has been told."
	}
	if event.SessionID != "" {
		if err := s.Library.SendMessage(ctx, event.SessionID, Title(src.Path), message); err != nil {
			s.Log.Printf("session message: %v", err)
		}
	}
	return nil
}

// libraryRelative turns a path as Jellyfin reports it into one relative to the
// library. Jellyfin may see the library elsewhere, such as inside a container.
func (s *Service) libraryRelative(itemPath string) (string, bool) {
	root := s.Cfg.JellyfinLibraryDir
	if root == "" {
		root = s.Cfg.LibraryDir
	}
	rel, err := filepath.Rel(root, itemPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}
