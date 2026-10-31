// Package notify sends short messages to Discord and ntfy.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Notifier delivers one message.
type Notifier interface {
	Send(ctx context.Context, message string) error
}

// Webhooks sends to every configured target. Either URL may be empty.
type Webhooks struct {
	DiscordURL string
	NtfyURL    string
	Client     *http.Client
}

// New returns a Webhooks with a bounded HTTP client.
func New(discordURL, ntfyURL string) *Webhooks {
	return &Webhooks{DiscordURL: discordURL, NtfyURL: ntfyURL, Client: &http.Client{Timeout: 15 * time.Second}}
}

// Send tries every target, so one failing does not silence the other.
func (w *Webhooks) Send(ctx context.Context, message string) error {
	var errs []error
	if w.DiscordURL != "" {
		// Discord rejects content over 2000 characters.
		body, err := json.Marshal(map[string]string{"content": truncate(message, 2000)})
		if err != nil {
			return err
		}
		errs = append(errs, w.post(ctx, w.DiscordURL, "application/json", body))
	}
	if w.NtfyURL != "" {
		errs = append(errs, w.post(ctx, w.NtfyURL, "text/plain", []byte(message)))
	}
	return errors.Join(errs...)
}

func (w *Webhooks) post(ctx context.Context, url, contentType string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := w.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("notify: %s", resp.Status)
	}
	return nil
}

func truncate(s string, limit int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= limit {
		return string(r)
	}
	return string(r[:limit-1]) + "…"
}
