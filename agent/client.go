package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/xartus2004/arena/episode"
)

// Client talks to an arena API server.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// APIError is a non-2xx response.
type APIError struct {
	Status  int
	Message string
	Infra   bool
}

func (e *APIError) Error() string { return fmt.Sprintf("arena api: %d: %s", e.Status, e.Message) }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("arena api: encode: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return fmt.Errorf("arena api: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("arena api: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var ae apiError
		_ = json.NewDecoder(resp.Body).Decode(&ae)
		return &APIError{Status: resp.StatusCode, Message: ae.Error, Infra: ae.Infra}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("arena api: decode: %w", err)
		}
	}
	return nil
}

// Create starts (or idempotently resumes) an episode.
func (c *Client) Create(ctx context.Context, req CreateRequest) (CreateResponse, error) {
	var out CreateResponse
	err := c.do(ctx, http.MethodPost, "/episodes", req, &out)
	return out, err
}

// Step sends one tool call.
func (c *Client) Step(ctx context.Context, id string, call episode.ToolCall) (episode.Observation, error) {
	var out StepResponse
	err := c.do(ctx, http.MethodPost, "/episodes/"+id+"/step", call, &out)
	return out.Observation, err
}

// Get fetches an episode.
func (c *Client) Get(ctx context.Context, id string) (EpisodeView, error) {
	var out CreateResponse
	err := c.do(ctx, http.MethodGet, "/episodes/"+id, nil, &out)
	return out.EpisodeView, err
}

// Drive runs policy against a remote episode until it is done.
func (c *Client) Drive(ctx context.Context, req CreateRequest, policy episode.Policy) (*episode.Outcome, error) {
	cr, err := c.Create(ctx, req)
	if err != nil {
		return nil, err
	}
	if cr.Outcome != nil {
		return cr.Outcome, nil
	}
	obs := *cr.Observation
	for !obs.Done {
		call, err := policy.Act(ctx, obs)
		if err != nil {
			return nil, fmt.Errorf("policy: %w", err)
		}
		obs, err = c.Step(ctx, cr.ID, call)
		if err != nil {
			return nil, err
		}
	}
	return obs.Outcome, nil
}
