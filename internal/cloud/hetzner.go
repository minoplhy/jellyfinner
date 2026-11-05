package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func init() { Register("hetzner", newHetzner) }

// hetzner is Hetzner Cloud. Labels select our servers, and the primary
// addresses Hetzner creates with a server are deleted with it.
type hetzner struct {
	s   Settings
	api *api
}

func newHetzner(s Settings) (Provider, error) {
	if s.Image == "" {
		// Hetzner's app image with Docker already installed.
		s.Image = "docker-ce"
	}
	base := s.BaseURL
	if base == "" {
		base = "https://api.hetzner.cloud/v1"
	}
	return &hetzner{s: s, api: newAPI(base, s.Token, s.HTTP, hetznerError)}, nil
}

func hetznerError(body []byte) (string, string) {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return e.Error.Code, e.Error.Message
}

// hetznerCodes sorts Hetzner's error codes. Quota and stock problems may come
// with any status, so the code decides.
var hetznerCodes = errorCodes{
	config:      []string{"invalid_input", "unauthorized", "forbidden", "token_readonly", "not_found"},
	unavailable: []string{"resource_unavailable", "resource_limit_exceeded", "placement_error", "rate_limit_exceeded", "server_error", "timeout", "maintenance", "unavailable", "conflict", "locked", "uniqueness_error"},
}

func (h *hetzner) Name() string { return "hetzner" }

func (h *hetzner) BillingStep() time.Duration { return time.Hour }

func (h *hetzner) Billing() Billing {
	return Billing{
		Note: "Billed by the hour, rounded up: a server that exists for a few minutes costs a whole hour. " +
			"A powered-off server is still billed until it is deleted. Each server's bill is capped at its monthly price.",
		Sources: []Source{{Title: "Hetzner Cloud billing FAQ", URL: "https://docs.hetzner.com/cloud/billing/faq/"}},
		Checked: "2026-11-05",
	}
}

type hetznerServer struct {
	ID         int64             `json:"id"`
	Name       string            `json:"name"`
	Created    time.Time         `json:"created"`
	Labels     map[string]string `json:"labels"`
	ServerType struct {
		Name string `json:"name"`
	} `json:"server_type"`
	Location struct {
		Name string `json:"name"`
	} `json:"location"`
	PublicNet struct {
		IPv4 *struct {
			ID int64 `json:"id"`
		} `json:"ipv4"`
		IPv6 *struct {
			ID int64 `json:"id"`
		} `json:"ipv6"`
	} `json:"public_net"`
}

func (s hetznerServer) server() Server {
	var ips []string
	if s.PublicNet.IPv4 != nil && s.PublicNet.IPv4.ID != 0 {
		ips = append(ips, strconv.FormatInt(s.PublicNet.IPv4.ID, 10))
	}
	if s.PublicNet.IPv6 != nil && s.PublicNet.IPv6.ID != 0 {
		ips = append(ips, strconv.FormatInt(s.PublicNet.IPv6.ID, 10))
	}
	return Server{
		ID: strconv.FormatInt(s.ID, 10), Name: s.Name, Type: s.ServerType.Name, Location: s.Location.Name,
		Resources: strings.Join(ips, ","), Cluster: s.Labels[LabelCluster], Created: s.Created,
	}
}

func (h *hetzner) Create(ctx context.Context, spec Spec) (Server, error) {
	body := map[string]any{
		"name":               spec.Name,
		"server_type":        spec.Type,
		"location":           spec.Location,
		"image":              h.s.Image,
		"user_data":          spec.UserData,
		"labels":             map[string]string{LabelRole: RoleWorker, LabelCluster: spec.Cluster},
		"start_after_create": true,
		"public_net":         map[string]bool{"enable_ipv4": true, "enable_ipv6": true},
	}
	if h.s.SSHKey != "" {
		body["ssh_keys"] = []string{h.s.SSHKey}
	}
	var out struct {
		Server hetznerServer `json:"server"`
	}
	if err := h.api.do(ctx, http.MethodPost, "/servers", body, &out); err != nil {
		return Server{}, classify(err, hetznerCodes)
	}
	return out.Server.server(), nil
}

func (h *hetzner) get(ctx context.Context, id string) (*hetznerServer, error) {
	var out struct {
		Server hetznerServer `json:"server"`
	}
	err := h.api.do(ctx, http.MethodGet, "/servers/"+url.PathEscape(id), nil, &out)
	if status(err) == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out.Server, nil
}

func (h *hetzner) Delete(ctx context.Context, s Server, cluster string) error {
	live, err := h.get(ctx, s.ID)
	if err != nil {
		return err
	}
	if live != nil {
		if live.Labels[LabelCluster] != cluster || cluster == "" {
			return fmt.Errorf("hetzner server %s: %w", s.ID, ErrForeign)
		}
		err := h.api.do(ctx, http.MethodDelete, "/servers/"+url.PathEscape(s.ID), nil, nil)
		if err != nil && status(err) != http.StatusNotFound {
			return err
		}
		return nil
	}
	// The server is gone. Its addresses go with it, unless something kept
	// them, and a kept address is billed too.
	for _, ip := range splitIDs(s.Resources) {
		err := h.api.do(ctx, http.MethodDelete, "/primary_ips/"+url.PathEscape(ip), nil, nil)
		if err != nil && status(err) != http.StatusNotFound {
			return err
		}
	}
	return nil
}

func (h *hetzner) Gone(ctx context.Context, s Server) (bool, error) {
	live, err := h.get(ctx, s.ID)
	if err != nil || live != nil {
		return false, err
	}
	for _, ip := range splitIDs(s.Resources) {
		err := h.api.do(ctx, http.MethodGet, "/primary_ips/"+url.PathEscape(ip), nil, nil)
		if status(err) == http.StatusNotFound {
			continue
		}
		return false, err
	}
	return true, nil
}

func (h *hetzner) List(ctx context.Context, cluster string) ([]Server, error) {
	var all []Server
	q := url.Values{"label_selector": {LabelCluster + "=" + cluster}, "per_page": {"50"}}
	for page := 1; ; page++ {
		q.Set("page", strconv.Itoa(page))
		var out struct {
			Servers []hetznerServer `json:"servers"`
			Meta    struct {
				Pagination struct {
					NextPage *int `json:"next_page"`
				} `json:"pagination"`
			} `json:"meta"`
		}
		if err := h.api.do(ctx, http.MethodGet, "/servers?"+q.Encode(), nil, &out); err != nil {
			return nil, err
		}
		for _, s := range out.Servers {
			if s.Labels[LabelCluster] == cluster {
				all = append(all, s.server())
			}
		}
		if out.Meta.Pagination.NextPage == nil || page >= 100 {
			return all, nil
		}
	}
}

type hetznerPrice struct {
	Location    string `json:"location"`
	PriceHourly struct {
		Net string `json:"net"`
	} `json:"price_hourly"`
}

func (h *hetzner) Prices(ctx context.Context) ([]Price, error) {
	var pricing struct {
		Pricing struct {
			Currency   string `json:"currency"`
			PrimaryIPs []struct {
				Type   string         `json:"type"`
				Prices []hetznerPrice `json:"prices"`
			} `json:"primary_ips"`
		} `json:"pricing"`
	}
	if err := h.api.do(ctx, http.MethodGet, "/pricing", nil, &pricing); err != nil {
		return nil, err
	}
	ipv4 := map[string]float64{}
	for _, ip := range pricing.Pricing.PrimaryIPs {
		if ip.Type != "ipv4" {
			continue
		}
		for _, p := range ip.Prices {
			v, err := strconv.ParseFloat(p.PriceHourly.Net, 64)
			if err != nil {
				return nil, fmt.Errorf("hetzner ipv4 price %q: %w", p.PriceHourly.Net, err)
			}
			ipv4[p.Location] = v
		}
	}
	var out []Price
	for _, t := range h.s.Types {
		var types struct {
			ServerTypes []struct {
				Name   string         `json:"name"`
				Prices []hetznerPrice `json:"prices"`
			} `json:"server_types"`
		}
		if err := h.api.do(ctx, http.MethodGet, "/server_types?name="+url.QueryEscape(t), nil, &types); err != nil {
			return nil, err
		}
		for _, st := range types.ServerTypes {
			for _, p := range st.Prices {
				if !contains(h.s.Locations, p.Location) {
					continue
				}
				v, err := strconv.ParseFloat(p.PriceHourly.Net, 64)
				if err != nil {
					return nil, fmt.Errorf("hetzner %s price %q: %w", st.Name, p.PriceHourly.Net, err)
				}
				out = append(out, Price{Type: st.Name, Location: p.Location, Hourly: v + ipv4[p.Location], Currency: pricing.Pricing.Currency})
			}
		}
	}
	return out, nil
}

func (h *hetzner) Check(ctx context.Context) error {
	var errs []error
	for _, t := range h.s.Types {
		var out struct {
			ServerTypes []json.RawMessage `json:"server_types"`
		}
		if err := h.api.do(ctx, http.MethodGet, "/server_types?name="+url.QueryEscape(t), nil, &out); err != nil {
			return err
		}
		if len(out.ServerTypes) == 0 {
			errs = append(errs, fmt.Errorf("server type %q does not exist", t))
		}
	}
	for _, l := range h.s.Locations {
		var out struct {
			Locations []json.RawMessage `json:"locations"`
		}
		if err := h.api.do(ctx, http.MethodGet, "/locations?name="+url.QueryEscape(l), nil, &out); err != nil {
			return err
		}
		if len(out.Locations) == 0 {
			errs = append(errs, fmt.Errorf("location %q does not exist", l))
		}
	}
	var images struct {
		Images []json.RawMessage `json:"images"`
	}
	if err := h.api.do(ctx, http.MethodGet, "/images?name="+url.QueryEscape(h.s.Image), nil, &images); err != nil {
		return err
	}
	if len(images.Images) == 0 {
		errs = append(errs, fmt.Errorf("image %q does not exist", h.s.Image))
	}
	return errors.Join(errs...)
}

func splitIDs(s string) []string {
	var out []string
	for _, id := range strings.Split(s, ",") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
