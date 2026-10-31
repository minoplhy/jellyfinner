// Package jellyfin talks to the Jellyfin server API.
package jellyfin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ErrBadLogin means Jellyfin rejected the username or password.
var ErrBadLogin = errors.New("bad login")

// Client calls one Jellyfin server.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// New returns a client with a bounded timeout.
func New(url, token string) *Client {
	return &Client{URL: url, Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf("MediaBrowser Token=%q", c.Token))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("jellyfin %s %s: %s", method, path, resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Check proves the server answers and accepts the token.
func (c *Client) Check(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/System/Info", nil, nil)
}

// Refresh asks Jellyfin to scan its libraries. The call returns once the scan
// is queued, not when it finishes.
func (c *Client) Refresh(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/Library/Refresh", nil, nil)
}

const clientHeader = `MediaBrowser Client="Jellyfinner", Device="orchestrator", DeviceId="jellyfinner-admin", Version="1"`

// Authenticate checks a login. It returns whether the user is an administrator,
// and a token for ending the session Jellyfin just created.
func (c *Client) Authenticate(ctx context.Context, username, password string) (bool, string, error) {
	body, err := json.Marshal(map[string]string{"Username": username, "Pw": password})
	if err != nil {
		return false, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/Users/AuthenticateByName", bytes.NewReader(body))
	if err != nil {
		return false, "", err
	}
	req.Header.Set("Authorization", clientHeader)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return false, "", ErrBadLogin
	}
	if resp.StatusCode >= 300 {
		return false, "", fmt.Errorf("jellyfin login: %s", resp.Status)
	}
	var out struct {
		AccessToken string `json:"AccessToken"`
		User        struct {
			Policy struct {
				IsAdministrator bool `json:"IsAdministrator"`
			} `json:"Policy"`
		} `json:"User"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, "", err
	}
	return out.User.Policy.IsAdministrator, out.AccessToken, nil
}

// EndSession signs out the session behind a token.
func (c *Client) EndSession(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/Sessions/Logout", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf("MediaBrowser Token=%q", token))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("jellyfin logout: %s", resp.Status)
	}
	return nil
}

// Username returns the owner of a viewer's token, using that token, so a bad
// one simply fails.
func (c *Client) Username(ctx context.Context, token string) (string, error) {
	viewer := Client{URL: c.URL, Token: token, HTTP: c.HTTP}
	var me struct {
		Name string `json:"Name"`
	}
	if err := viewer.do(ctx, http.MethodGet, "/Users/Me", nil, &me); err != nil {
		return "", err
	}
	return me.Name, nil
}

// ItemPath returns an item's file path as Jellyfin sees it.
func (c *Client) ItemPath(ctx context.Context, itemID string) (string, error) {
	query := url.Values{"ids": {itemID}, "fields": {"Path"}, "recursive": {"true"}}
	var resp struct {
		Items []struct {
			Path string `json:"Path"`
		} `json:"Items"`
	}
	if err := c.do(ctx, http.MethodGet, "/Items?"+query.Encode(), nil, &resp); err != nil {
		return "", err
	}
	if len(resp.Items) == 0 {
		return "", fmt.Errorf("jellyfin: no item %s", itemID)
	}
	return resp.Items[0].Path, nil
}

// ClearPlayed removes a user's played flag from an item.
func (c *Client) ClearPlayed(ctx context.Context, itemID, userID string) error {
	path := "/UserPlayedItems/" + url.PathEscape(itemID) + "?" + url.Values{"userId": {userID}}.Encode()
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// SendMessage shows a message on a viewer's session.
func (c *Client) SendMessage(ctx context.Context, sessionID, header, text string) error {
	body, err := json.Marshal(map[string]any{"Header": header, "Text": text, "TimeoutMs": 8000})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/Sessions/"+url.PathEscape(sessionID)+"/Message", body, nil)
}
