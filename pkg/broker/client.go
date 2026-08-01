package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is the console's side of the broker API.
//
// It carries no credentials of its own: every call forwards the OPERATOR's
// bearer token, so the broker authorizes the human, not the console. A
// console without a live operator session can ask the broker for nothing.
type Client struct {
	baseURL string
	http    *http.Client
}

// clientTimeout bounds one broker call. A plan is a directory refresh plus
// a full organization read on a cold cache; a minute is generous without
// being an eternity.
const clientTimeout = time.Minute

// NewClient points a client at a broker.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: clientTimeout},
	}
}

// ErrDrift is returned by Apply when the organization changed since the
// plan was reviewed. Fresh carries the plan to re-review.
type ErrDrift struct {
	Fresh *PlanResponse
}

func (e *ErrDrift) Error() string {
	return "the organization changed since this plan was reviewed"
}

// Plan asks the broker to compute and store a plan.
func (c *Client) Plan(ctx context.Context, org, token string) (*PlanResponse, error) {
	var out PlanResponse
	if err := c.call(ctx, http.MethodPost, fmt.Sprintf("/v1/orgs/%s/plans", org), token, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// GetPlan re-reads a stored plan.
func (c *Client) GetPlan(ctx context.Context, org, hash, token string) (*PlanResponse, error) {
	var out PlanResponse
	if err := c.call(ctx, http.MethodGet, fmt.Sprintf("/v1/orgs/%s/plans/%s", org, hash), token, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// Apply asks the broker to execute a reviewed plan. A drift refusal comes
// back as *ErrDrift carrying the fresh plan.
func (c *Client) Apply(ctx context.Context, org, hash, token string) (*ApplyResponse, error) {
	var out ApplyResponse

	err := c.call(ctx, http.MethodPost, fmt.Sprintf("/v1/orgs/%s/plans/%s/apply", org, hash), token, &out)
	if err != nil {
		return nil, err
	}

	return &out, nil
}

// call performs one request and decodes the answer or the error envelope.
func (c *Client) call(ctx context.Context, method, path, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, http.NoBody)
	if err != nil {
		return fmt.Errorf("build broker request: %w", err)
	}

	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call broker: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read broker response: %w", err)
	}

	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decode broker response: %w", err)
		}

		return nil
	}

	// Error envelope. A 409 carries the fresh plan to re-review; anything
	// else carries a message.
	var envelope struct {
		Error string        `json:"error"`
		Plan  *PlanResponse `json:"plan"`
	}

	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Error == "" {
		return fmt.Errorf("broker returned %s: %s", resp.Status, bytes.TrimSpace(body))
	}

	if resp.StatusCode == http.StatusConflict && envelope.Plan != nil {
		return &ErrDrift{Fresh: envelope.Plan}
	}

	return fmt.Errorf("broker: %s", envelope.Error)
}
