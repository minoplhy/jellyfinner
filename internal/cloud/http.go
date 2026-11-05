package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// APIError is a provider's refusal, with its status and own error code.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("HTTP %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

// api is a small JSON client shared by the adapters.
type api struct {
	base  string
	token string
	http  *http.Client
	// decodeError reads a provider's error body into code and message.
	decodeError func(body []byte) (code, message string)

	// attempts and wait bound the retries of 429 and, for requests that are
	// safe to repeat, 5xx and network errors.
	attempts int
	wait     time.Duration
}

func newAPI(base, token string, client *http.Client, decode func([]byte) (string, string)) *api {
	return &api{base: base, token: token, http: client, decodeError: decode, attempts: 4, wait: time.Second}
}

// do sends a request and decodes a JSON reply into out, if given. A create is
// not repeated after a 5xx or a dropped connection, because it may have
// worked; reconcile finds such a server by its label.
func (a *api) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	repeatable := method == http.MethodGet || method == http.MethodDelete
	var last error
	for attempt := range a.attempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(a.wait << (attempt - 1)):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, a.base+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+a.token)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := a.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			last = fmt.Errorf("%s %s: %w", method, path, err)
			if repeatable {
				continue
			}
			return last
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			last = err
			if repeatable {
				continue
			}
			return last
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out == nil || len(data) == 0 {
				return nil
			}
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("%s %s: decode reply: %w", method, path, err)
			}
			return nil
		}
		apiErr := &APIError{Status: resp.StatusCode}
		apiErr.Code, apiErr.Message = a.decodeError(data)
		if apiErr.Message == "" {
			apiErr.Message = http.StatusText(resp.StatusCode)
		}
		last = fmt.Errorf("%s %s: %w", method, path, apiErr)
		retry := resp.StatusCode == http.StatusTooManyRequests || (repeatable && resp.StatusCode >= 500)
		if !retry {
			return last
		}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 && s <= 60 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(s) * time.Second):
			}
		}
	}
	return last
}

// status returns the HTTP status of a provider refusal, or 0.
func status(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

// code returns the provider's own error code, or "".
func code(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

// errorCodes lists a provider's own error codes by meaning. They come before
// the HTTP status, because a provider may report a quota with 403.
type errorCodes struct {
	config      []string
	unavailable []string
}

// classify wraps an error from a create so the scaler knows whether to move
// on. Auth and validation problems are configuration; everything else,
// including codes not known here, is treated as unavailable, so an unexpected
// refusal costs a cooldown rather than stopping the provider for good.
func classify(err error, codes errorCodes) error {
	if err == nil {
		return nil
	}
	if s := status(err); s == 0 {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	c := code(err)
	if c != "" && contains(codes.unavailable, c) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if c != "" && contains(codes.config, c) {
		return fmt.Errorf("%w: %w", ErrConfig, err)
	}
	if s := status(err); s == http.StatusUnauthorized || s == http.StatusForbidden {
		return fmt.Errorf("%w: %w", ErrConfig, err)
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}
