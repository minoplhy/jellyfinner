package cloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func init() { Register("cherry", newCherry) }

// cherry is Cherry Servers cloud VPS on hourly billing. Servers live in a
// project, and tags mark ours.
type cherry struct {
	s   Settings
	api *api
}

func newCherry(s Settings) (Provider, error) {
	if s.Project == "" {
		return nil, errors.New("cherry: CLOUD_CHERRY_PROJECT is required, the ID of the project servers go in")
	}
	if s.Image == "" {
		return nil, errors.New("cherry: CLOUD_CHERRY_IMAGE is required, an image slug the plan offers, with Docker or apt")
	}
	base := s.BaseURL
	if base == "" {
		base = "https://api.cherryservers.com/v1"
	}
	return &cherry{s: s, api: newAPI(base, s.Token, s.HTTP, cherryError)}, nil
}

func cherryError(body []byte) (string, string) {
	var e struct {
		Code    json.Number `json:"code"`
		Message string      `json:"message"`
		Error   string      `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Message == "" {
		e.Message = e.Error
	}
	return e.Code.String(), e.Message
}

func (c *cherry) Name() string { return "cherry" }

// BillingStep assumes whole hours, the costlier case, because Cherry Servers
// does not document how a partial hour is billed.
func (c *cherry) BillingStep() time.Duration { return time.Hour }

func (c *cherry) Billing() Billing {
	return Billing{
		Note: "Billed hourly from the team balance. How a partial hour is billed is not documented, so Jellyfinner " +
			"assumes a whole hour, the costlier case. A powered-off server is still billed until it is terminated.",
		Sources: []Source{
			{Title: "Cherry Servers: server actions", URL: "https://www.cherryservers.com/knowledge/docs/compute/configuration-management/server-actions"},
			{Title: "Cherry Servers: Cloud VPS", URL: "https://www.cherryservers.com/knowledge/docs/compute/virtual-servers/cloud-vps"},
		},
		Checked: "2026-11-05",
	}
}

type cherryServer struct {
	ID        int64             `json:"id"`
	Hostname  string            `json:"hostname"`
	Tags      map[string]string `json:"tags"`
	CreatedAt string            `json:"created_at"`
	Region    struct {
		Slug string `json:"slug"`
	} `json:"region"`
	Plan struct {
		Slug string `json:"slug"`
	} `json:"plan"`
}

func (s cherryServer) server() Server {
	created, _ := time.Parse(time.RFC3339, s.CreatedAt)
	return Server{
		ID: strconv.FormatInt(s.ID, 10), Name: s.Hostname, Type: s.Plan.Slug, Location: s.Region.Slug,
		Cluster: s.Tags[LabelCluster], Created: created,
	}
}

func (c *cherry) Create(ctx context.Context, spec Spec) (Server, error) {
	body := map[string]any{
		"plan":        spec.Type,
		"region":      spec.Location,
		"image":       c.s.Image,
		"hostname":    spec.Name,
		"user_data":   base64.StdEncoding.EncodeToString([]byte(spec.UserData)),
		"tags":        map[string]string{LabelRole: RoleWorker, LabelCluster: spec.Cluster},
		"spot_market": false,
		"cycle":       "hourly",
	}
	if c.s.SSHKey != "" {
		if id, err := strconv.Atoi(c.s.SSHKey); err == nil {
			body["ssh_keys"] = []int{id}
		} else {
			body["ssh_keys"] = []string{c.s.SSHKey}
		}
	}
	var out cherryServer
	if err := c.api.do(ctx, http.MethodPost, "/projects/"+url.PathEscape(c.s.Project)+"/servers", body, &out); err != nil {
		if status(err) == http.StatusNotFound {
			return Server{}, fmt.Errorf("%w: project %s: %w", ErrConfig, c.s.Project, err)
		}
		return Server{}, classify(err, errorCodes{})
	}
	srv := out.server()
	if srv.Type == "" {
		srv.Type = spec.Type
	}
	if srv.Location == "" {
		srv.Location = spec.Location
	}
	return srv, nil
}

func (c *cherry) get(ctx context.Context, id string) (*cherryServer, error) {
	var out cherryServer
	err := c.api.do(ctx, http.MethodGet, "/servers/"+url.PathEscape(id), nil, &out)
	if status(err) == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *cherry) Delete(ctx context.Context, s Server, cluster string) error {
	live, err := c.get(ctx, s.ID)
	if err != nil || live == nil {
		return err
	}
	if live.Tags[LabelCluster] != cluster || cluster == "" {
		return fmt.Errorf("cherry server %s: %w", s.ID, ErrForeign)
	}
	err = c.api.do(ctx, http.MethodDelete, "/servers/"+url.PathEscape(s.ID), nil, nil)
	if err != nil && status(err) != http.StatusNotFound {
		return err
	}
	return nil
}

func (c *cherry) Gone(ctx context.Context, s Server) (bool, error) {
	live, err := c.get(ctx, s.ID)
	return err == nil && live == nil, err
}

// List pages through the project, since the API has no tag filter.
func (c *cherry) List(ctx context.Context, cluster string) ([]Server, error) {
	const limit = 100
	var all []Server
	for offset := 0; offset < 100*limit; offset += limit {
		var page []cherryServer
		path := fmt.Sprintf("/projects/%s/servers?limit=%d&offset=%d", url.PathEscape(c.s.Project), limit, offset)
		if err := c.api.do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		for _, s := range page {
			if s.Tags[LabelCluster] == cluster && cluster != "" {
				all = append(all, s.server())
			}
		}
		if len(page) < limit {
			break
		}
	}
	return all, nil
}

type cherryPlan struct {
	Slug    string `json:"slug"`
	Pricing []struct {
		Price    float64 `json:"price"`
		Currency string  `json:"currency"`
		Taxed    bool    `json:"taxed"`
		Unit     string  `json:"unit"`
	} `json:"pricing"`
	AvailableRegions []struct {
		Slug string `json:"slug"`
	} `json:"available_regions"`
}

func (c *cherry) plan(ctx context.Context, slug string) (*cherryPlan, error) {
	var p cherryPlan
	if err := c.api.do(ctx, http.MethodGet, "/plans/"+url.PathEscape(slug), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *cherry) Prices(ctx context.Context) ([]Price, error) {
	var out []Price
	for _, t := range c.s.Types {
		p, err := c.plan(ctx, t)
		if err != nil {
			return nil, err
		}
		for _, pr := range p.Pricing {
			if !strings.EqualFold(pr.Unit, "hourly") {
				continue
			}
			for _, l := range c.s.Locations {
				out = append(out, Price{Type: t, Location: l, Hourly: pr.Price, Currency: pr.Currency, Taxed: pr.Taxed})
			}
			break
		}
	}
	return out, nil
}

func (c *cherry) Check(ctx context.Context) error {
	if err := c.api.do(ctx, http.MethodGet, "/projects/"+url.PathEscape(c.s.Project), nil, nil); err != nil {
		return fmt.Errorf("project %s: %w", c.s.Project, err)
	}
	var errs []error
	for _, t := range c.s.Types {
		p, err := c.plan(ctx, t)
		if status(err) == http.StatusNotFound {
			errs = append(errs, fmt.Errorf("plan %q does not exist", t))
			continue
		}
		if err != nil {
			return err
		}
		if len(p.AvailableRegions) > 0 {
			for _, l := range c.s.Locations {
				found := false
				for _, r := range p.AvailableRegions {
					found = found || r.Slug == l
				}
				if !found {
					errs = append(errs, fmt.Errorf("plan %q is not offered in region %q", t, l))
				}
			}
		}
		var images []struct {
			Slug string `json:"slug"`
		}
		if err := c.api.do(ctx, http.MethodGet, "/plans/"+url.PathEscape(t)+"/images", nil, &images); err != nil {
			return err
		}
		found := false
		for _, img := range images {
			found = found || img.Slug == c.s.Image
		}
		if !found {
			errs = append(errs, fmt.Errorf("plan %q does not offer image %q", t, c.s.Image))
		}
	}
	return errors.Join(errs...)
}
