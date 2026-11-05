// Package jellyfintest helps tests fake a Jellyfin server.
package jellyfintest

import (
	"context"
	"errors"

	"jellyfinner/internal/jellyfin"
)

// ErrNotFaked is what an Unimplemented method returns.
var ErrNotFaked = errors.New("jellyfintest: method not faked")

// Unimplemented satisfies jellyfin.API with methods that fail. A fake embeds
// it and overrides only what its test needs, so it keeps compiling when the
// API grows.
type Unimplemented struct{}

var _ jellyfin.API = Unimplemented{}

func (Unimplemented) Check(context.Context) error   { return ErrNotFaked }
func (Unimplemented) Refresh(context.Context) error { return ErrNotFaked }
func (Unimplemented) Authenticate(context.Context, string, string) (bool, string, error) {
	return false, "", ErrNotFaked
}
func (Unimplemented) EndSession(context.Context, string) error { return ErrNotFaked }
func (Unimplemented) Username(context.Context, string) (string, error) {
	return "", ErrNotFaked
}
func (Unimplemented) ItemPath(context.Context, string) (string, error) {
	return "", ErrNotFaked
}
func (Unimplemented) ClearPlayed(context.Context, string, string) error { return ErrNotFaked }
func (Unimplemented) SendMessage(context.Context, string, string, string) error {
	return ErrNotFaked
}
func (Unimplemented) Users(context.Context) ([]string, error) { return nil, ErrNotFaked }
