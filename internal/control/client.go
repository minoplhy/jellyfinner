// Package control is the client side of the control API. It stays free of the
// server's database dependency, so nodes can be static binaries.
package control

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"jellyfinner/internal/buildinfo"
	"jellyfinner/internal/config"
	"jellyfinner/internal/job"
)

// ClaimResponse hands a job to a worker.
type ClaimResponse struct {
	Job   job.Job `json:"job"`
	Lease string  `json:"lease"`
}

// ClaimRequest asks for work, and tells the orchestrator what this worker is.
// An empty body is still accepted, so older workers keep working.
type ClaimRequest struct {
	Info *WorkerInfo `json:"info,omitempty"`
}

// WorkerInfo describes a worker's build and hardware. It is gathered once at
// start and sent with claims and heartbeats, so an orchestrator that restarts
// learns it again within one call, even while a long encode runs.
type WorkerInfo struct {
	CPU      string  `json:"cpu,omitempty"`       // CPU model, or the board on Arm
	Cores    int     `json:"cores,omitempty"`     // physical cores, 0 when unknown
	Threads  int     `json:"threads,omitempty"`   // logical CPUs this process may use
	CPULimit float64 `json:"cpu_limit,omitempty"` // container CPU quota in CPUs, 0 for none
	Backend  string  `json:"backend,omitempty"`   // configured encoder: software, vaapi or rkmpp
	Device   string  `json:"device,omitempty"`    // VAAPI render node, when used
	FFmpeg   string  `json:"ffmpeg,omitempty"`    // ffmpeg's version line
	Kernel   string  `json:"kernel,omitempty"`
	OS       string  `json:"os,omitempty"` // GOOS/GOARCH
}

// HeartbeatRequest extends a lease. Backend is the encoder running right now,
// which differs from the configured one after a fall back to software, and is
// "copy" for a stream copy.
type HeartbeatRequest struct {
	Lease    string      `json:"lease"`
	Stage    string      `json:"stage"`
	Fraction float64     `json:"fraction"`
	Backend  string      `json:"backend,omitempty"`
	Info     *WorkerInfo `json:"info,omitempty"`
}

// NodeReport is what the orchestrator last heard from a node. It lives in
// memory only, so it is empty after a restart until the node calls again.
type NodeReport struct {
	UserAgent string
	Version   string      // from the User-Agent, "" for an older node
	Platform  string      // from the User-Agent, such as "linux/arm64; go1.27.3"
	Worker    *WorkerInfo // nil until a worker reports it
	// The job a worker is running, from its heartbeats.
	Job         string
	Stage       string
	Backend     string    // encoder in use now: software, vaapi, rkmpp or copy
	EncodeSince time.Time // when Stage last became encode
}

// ParseUserAgent splits "jellyfinner-role/version (platform)" into the
// version and platform. Anything else, such as an older node's Go default,
// gives empty strings.
func ParseUserAgent(ua string) (version, platform string) {
	product, rest, _ := strings.Cut(ua, " ")
	name, version, ok := strings.Cut(product, "/")
	if !ok || !strings.HasPrefix(name, "jellyfinner-") {
		return "", ""
	}
	if strings.HasPrefix(rest, "(") && strings.HasSuffix(rest, ")") {
		platform = rest[1 : len(rest)-1]
	}
	return version, platform
}

// HeartbeatResponse tells a worker whether to stop.
type HeartbeatResponse struct {
	Cancel bool `json:"cancel"`
}

// FinishRequest ends a job, successfully when Error is empty. Released hands it
// back unharmed, because the worker is stopping rather than the job failing.
type FinishRequest struct {
	Lease    string          `json:"lease"`
	Released bool            `json:"released,omitempty"`
	Manifest json.RawMessage `json:"manifest,omitempty"`
	Error    string          `json:"error,omitempty"`
	Log      string          `json:"log,omitempty"`
}

// aliveFile lets a container health check see that a node is reaching the
// orchestrator, without opening a port.
func aliveFile() string { return filepath.Join(os.TempDir(), "jellyfinner-alive") }

// MarkAlive records a successful call to the orchestrator.
func MarkAlive() {
	if err := os.WriteFile(aliveFile(), nil, 0o600); err != nil {
		return
	}
	now := time.Now()
	_ = os.Chtimes(aliveFile(), now, now)
}

// CheckAlive fails if the last successful call is older than maxAge.
func CheckAlive(maxAge time.Duration) error {
	info, err := os.Stat(aliveFile())
	if err != nil {
		return errors.New("no contact with the orchestrator yet")
	}
	if age := time.Since(info.ModTime()); age > maxAge {
		return fmt.Errorf("last contact with the orchestrator was %s ago", age.Round(time.Second))
	}
	return nil
}

// ErrLeaseLost means the orchestrator gave the job away, so the worker must
// stop and drop its result.
var ErrLeaseLost = errors.New("lease lost")

// ErrTooLarge means something on the path, usually a reverse proxy, refused
// the request body as too large. Smaller requests can still get through.
var ErrTooLarge = errors.New("request body too large")

// Headers used by file transfers.
const (
	LeaseHeader  = "X-Job-Lease"
	OffsetHeader = "Upload-Offset"
)

// Client is a node's connection to the control API.
type Client struct {
	// Role names the program in the User-Agent, which is how the orchestrator
	// learns each node's version.
	Role     string
	url      string
	token    string
	http     *http.Client
	transfer *http.Client
}

// NewClient refuses a URL that breaks the transport rule. TLS verification is
// Go's default, and there is deliberately no way to turn it off.
func NewClient(baseURL, token string) (*Client, error) {
	if err := config.CheckControlURL(baseURL); err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("control token is required")
	}
	return &Client{
		Role:     "node",
		url:      strings.TrimSuffix(baseURL, "/"),
		token:    token,
		http:     &http.Client{Timeout: 30 * time.Second},
		transfer: &http.Client{Transport: transferTransport()},
	}, nil
}

// transferTransport moves whole files, so it has no overall timeout. It keeps
// to HTTP/1.1, because HTTP/2 would put every parallel stream on one TCP
// connection, which is the limit parallel streams exist to get around. It
// names HTTP/1.1 in the TLS handshake: offered nothing, a server with HTTP/2
// enabled, such as nginx with http2 on, may answer in HTTP/2 anyway.
func transferTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Protocols = new(http.Protocols)
	t.Protocols.SetHTTP1(true)
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{}
	}
	t.TLSClientConfig.NextProtos = []string{"http/1.1"}
	t.MaxIdleConnsPerHost = 32
	t.ReadBufferSize = 1 << 20
	t.WriteBufferSize = 1 << 20
	return t
}

// newRequest adds the token and User-Agent every call carries.
func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", buildinfo.UserAgent(c.Role))
	return req, nil
}

func (c *Client) transferRequest(ctx context.Context, method, path, lease string, body io.Reader) (*http.Request, error) {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(LeaseHeader, lease)
	return req, nil
}

func transferError(path string, resp *http.Response) error {
	if resp.StatusCode == http.StatusConflict {
		return ErrLeaseLost
	}
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		return fmt.Errorf("control api %s: %s: %w", path, resp.Status, ErrTooLarge)
	}
	return fmt.Errorf("control api %s: %s", path, resp.Status)
}

// SourceSize returns the size of a job's source.
func (c *Client) SourceSize(ctx context.Context, id, lease string) (int64, error) {
	path := "/v1/jobs/" + id + "/source"
	req, err := c.transferRequest(ctx, http.MethodHead, path, lease, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.transfer.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, transferError(path, resp)
	}
	if resp.ContentLength < 0 {
		return 0, errors.New("source has no length")
	}
	return resp.ContentLength, nil
}

// SourceRange opens length bytes of a job's source from offset.
func (c *Client) SourceRange(ctx context.Context, id, lease string, offset, length int64) (io.ReadCloser, error) {
	path := "/v1/jobs/" + id + "/source"
	req, err := c.transferRequest(ctx, http.MethodGet, path, lease, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	resp, err := c.transfer.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusPartialContent {
		defer resp.Body.Close()
		return nil, transferError(path, resp)
	}
	return resp.Body, nil
}

// UploadOffset returns how much of a file the orchestrator holds.
func (c *Client) UploadOffset(ctx context.Context, id, lease, name string) (int64, error) {
	path := "/v1/jobs/" + id + "/files/" + url.PathEscape(name)
	req, err := c.transferRequest(ctx, http.MethodHead, path, lease, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.transfer.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return 0, transferError(path, resp)
	}
	return strconv.ParseInt(resp.Header.Get(OffsetHeader), 10, 64)
}

// Upload sends size bytes of a file from offset. Offset 0 starts it again.
func (c *Client) Upload(ctx context.Context, id, lease, name string, offset int64, body io.Reader, size int64) error {
	path := "/v1/jobs/" + id + "/files/" + url.PathEscape(name)
	req, err := c.transferRequest(ctx, http.MethodPut, path, lease, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	if size == 0 {
		req.Body = http.NoBody
	}
	req.Header.Set(OffsetHeader, strconv.FormatInt(offset, 10))
	resp, err := c.transfer.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return transferError(path, resp)
	}
	if got := resp.Header.Get(OffsetHeader); got != strconv.FormatInt(offset+size, 10) {
		return fmt.Errorf("upload of %s: orchestrator holds %s bytes", name, got)
	}
	return nil
}

func (c *Client) post(ctx context.Context, path string, body, out any) (int, error) {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		payload = bytes.NewReader(data)
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, payload)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusConflict:
		return resp.StatusCode, ErrLeaseLost
	case resp.StatusCode >= 300:
		return resp.StatusCode, fmt.Errorf("control api %s: %s", path, resp.Status)
	case out != nil && resp.StatusCode == http.StatusOK:
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}

// WhoAmIResponse names the node a token belongs to.
type WhoAmIResponse struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// WhoAmI tests the connection and token, and changes nothing.
func (c *Client) WhoAmI(ctx context.Context) (*WhoAmIResponse, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/whoami", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("control api /v1/whoami: %s", resp.Status)
	}
	var out WhoAmIResponse
	return &out, json.NewDecoder(resp.Body).Decode(&out)
}

// RouteRequest asks where a Jellyfin user should stream a library file from.
// Output is the file's path inside the library, with forward slashes.
type RouteRequest struct {
	Username string `json:"username"`
	Output   string `json:"output"`
}

// RouteResponse is a signed edge URL, or empty for the origin.
type RouteResponse struct {
	URL string `json:"url"`
}

// Resolve asks the orchestrator where to send a viewer. An empty URL means
// the origin.
func (c *Client) Resolve(ctx context.Context, username, output string) (string, error) {
	var resp RouteResponse
	if _, err := c.post(ctx, "/v1/router/resolve", RouteRequest{Username: username, Output: output}, &resp); err != nil {
		return "", err
	}
	return resp.URL, nil
}

// Claim asks for work, reporting info when not nil. It returns nil when the
// queue is empty.
func (c *Client) Claim(ctx context.Context, info *WorkerInfo) (*ClaimResponse, error) {
	var resp ClaimResponse
	var body any
	if info != nil {
		body = ClaimRequest{Info: info}
	}
	status, err := c.post(ctx, "/v1/jobs/claim", body, &resp)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	return &resp, nil
}

// Heartbeat extends a lease and reports whether the job was cancelled.
func (c *Client) Heartbeat(ctx context.Context, id string, req HeartbeatRequest) (bool, error) {
	var resp HeartbeatResponse
	if _, err := c.post(ctx, "/v1/jobs/"+id+"/heartbeat", req, &resp); err != nil {
		return false, err
	}
	return resp.Cancel, nil
}

// Finish reports a job's outcome.
func (c *Client) Finish(ctx context.Context, id string, req FinishRequest) error {
	_, err := c.post(ctx, "/v1/jobs/"+id+"/finish", req, nil)
	return err
}

// Order verbs. There are deliberately only two.
const (
	VerbPull   = "pull"
	VerbDelete = "delete"
)

// EdgePollRequest is an agent's report.
type EdgePollRequest struct {
	DiskFree  int64    `json:"disk_free"`
	DiskTotal int64    `json:"disk_total"`
	Files     []string `json:"files"`
}

// EdgeOrder asks a node to act on one file. It carries no path or URL.
type EdgeOrder struct {
	Verb   string `json:"verb"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size,omitempty"`
	Token  string `json:"token,omitempty"`
}

// EdgePollResponse lists a node's orders.
type EdgePollResponse struct {
	Orders []EdgeOrder `json:"orders"`
}

// EdgeAck reports an order's outcome. An empty Error means success.
type EdgeAck struct {
	Verb   string `json:"verb"`
	SHA256 string `json:"sha256"`
	Error  string `json:"error,omitempty"`
}

// EdgePoll reports to the orchestrator and fetches orders.
func (c *Client) EdgePoll(ctx context.Context, req EdgePollRequest) (*EdgePollResponse, error) {
	var resp EdgePollResponse
	if _, err := c.post(ctx, "/v1/edge/poll", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// EdgeAck reports an order's outcome.
func (c *Client) EdgeAck(ctx context.Context, req EdgeAck) error {
	_, err := c.post(ctx, "/v1/edge/ack", req, nil)
	return err
}

// EdgeDownload opens a file on the orchestrator from an offset. The URL is always built from
// the configured address, never from an order. It has no overall timeout,
// because a capped pull can legitimately take hours.
func (c *Client) EdgeDownload(ctx context.Context, hash, token string, offset int64) (*http.Response, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/edge/files/"+hash, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Download-Token", token)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	return c.transfer.Do(req)
}

// API is everything a node asks of the control API. Client is the real one;
// worker, edge agent and transfer code depend on this interface instead.
type API interface {
	// WhoAmI returns the node this token belongs to.
	WhoAmI(ctx context.Context) (*WhoAmIResponse, error)
	// Claim asks for a job, reporting info when not nil, and returns nil
	// when there is none.
	Claim(ctx context.Context, info *WorkerInfo) (*ClaimResponse, error)
	// Heartbeat extends a lease, and reports true when the job was cancelled.
	Heartbeat(ctx context.Context, id string, req HeartbeatRequest) (bool, error)
	// Finish reports a job's result.
	Finish(ctx context.Context, id string, req FinishRequest) error

	// SourceSize returns the size of a job's source file.
	SourceSize(ctx context.Context, id, lease string) (int64, error)
	// SourceRange streams part of a job's source file.
	SourceRange(ctx context.Context, id, lease string, offset, length int64) (io.ReadCloser, error)
	// UploadOffset returns how much of an output file has been received.
	UploadOffset(ctx context.Context, id, lease, name string) (int64, error)
	// Upload sends part of an output file, starting at offset.
	Upload(ctx context.Context, id, lease, name string, offset int64, body io.Reader, size int64) error

	// EdgePoll reports an edge node's state and returns its orders.
	EdgePoll(ctx context.Context, req EdgePollRequest) (*EdgePollResponse, error)
	// EdgeAck reports the result of an order.
	EdgeAck(ctx context.Context, req EdgeAck) error
	// EdgeDownload fetches an encoded file, from offset.
	EdgeDownload(ctx context.Context, hash, token string, offset int64) (*http.Response, error)
}

var _ API = (*Client)(nil)
