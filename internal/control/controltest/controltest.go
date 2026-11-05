// Package controltest helps tests fake the control API.
package controltest

import (
	"context"
	"errors"
	"io"
	"net/http"

	"jellyfinner/internal/control"
)

// ErrNotFaked is what an Unimplemented method returns.
var ErrNotFaked = errors.New("controltest: method not faked")

// Unimplemented satisfies control.API with methods that fail. A fake embeds
// it and overrides only what its test needs.
type Unimplemented struct{}

var _ control.API = Unimplemented{}

func (Unimplemented) WhoAmI(context.Context) (*control.WhoAmIResponse, error) {
	return nil, ErrNotFaked
}
func (Unimplemented) Claim(context.Context) (*control.ClaimResponse, error) { return nil, ErrNotFaked }
func (Unimplemented) Heartbeat(context.Context, string, control.HeartbeatRequest) (bool, error) {
	return false, ErrNotFaked
}
func (Unimplemented) Finish(context.Context, string, control.FinishRequest) error {
	return ErrNotFaked
}
func (Unimplemented) SourceSize(context.Context, string, string) (int64, error) {
	return 0, ErrNotFaked
}
func (Unimplemented) SourceRange(context.Context, string, string, int64, int64) (io.ReadCloser, error) {
	return nil, ErrNotFaked
}
func (Unimplemented) UploadOffset(context.Context, string, string, string) (int64, error) {
	return 0, ErrNotFaked
}
func (Unimplemented) Upload(context.Context, string, string, string, int64, io.Reader, int64) error {
	return ErrNotFaked
}
func (Unimplemented) EdgePoll(context.Context, control.EdgePollRequest) (*control.EdgePollResponse, error) {
	return nil, ErrNotFaked
}
func (Unimplemented) EdgeAck(context.Context, control.EdgeAck) error { return ErrNotFaked }
func (Unimplemented) EdgeDownload(context.Context, string, string, int64) (*http.Response, error) {
	return nil, ErrNotFaked
}
