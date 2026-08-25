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
		if out == nil {
			return nil // caller does not want the body
		}

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

// ApplyAsync starts an apply and returns the broker's run id; the
// transcript streams from StreamRunURL's endpoint.
func (c *Client) ApplyAsync(ctx context.Context, org, hash, token string) (string, error) {
	var out struct {
		RunID string `json:"runId"`
	}

	err := c.call(ctx, http.MethodPost, fmt.Sprintf("/v1/orgs/%s/plans/%s/apply-async", org, hash), token, &out)
	if err != nil {
		return "", err
	}

	if out.RunID == "" {
		return "", fmt.Errorf("broker returned no run id")
	}

	return out.RunID, nil
}

// StreamRun opens the run's SSE stream. The caller owns the response
// body and must close it; the client's normal timeout does not apply —
// a stream lives as long as the run narrates.
func (c *Client) StreamRun(ctx context.Context, org, id, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/orgs/%s/runs/%s/stream", c.baseURL, org, id), http.NoBody)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", token)

	streaming := &http.Client{} // no timeout: the stream outlives any sane one

	resp, err := streaming.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open run stream: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()

		return nil, fmt.Errorf("run stream: %s", resp.Status)
	}

	return resp, nil
}

// ReconcileStatus fetches the broker's per-organization reconcile status
// for the console's Status page.
func (c *Client) ReconcileStatus(ctx context.Context, token string) ([]ReconcileStatus, error) {
	var out []ReconcileStatus
	if err := c.call(ctx, http.MethodGet, "/v1/reconcile/status", token, &out); err != nil {
		return nil, err
	}

	return out, nil
}

// RunReconcile triggers one reconcile pass (the Status page's "Sync now").
func (c *Client) RunReconcile(ctx context.Context, token string) error {
	var out []ReconcileStatus
	return c.call(ctx, http.MethodPost, "/v1/reconcile", token, &out)
}

// SetPaused pauses or resumes an organization's reconcile loop.
func (c *Client) SetPaused(ctx context.Context, org string, paused bool, token string) error {
	verb := "unpause"
	if paused {
		verb = "pause"
	}
	return c.call(ctx, http.MethodPost, fmt.Sprintf("/v1/orgs/%s/%s", org, verb), token, nil)
}

// SetReconcileEnabled turns an organization's reconcile loop on or off — the
// operator's UI override of the config day-0 default.
func (c *Client) SetReconcileEnabled(ctx context.Context, org string, enabled bool, token string) error {
	verb := "disable"
	if enabled {
		verb = "enable"
	}
	return c.call(ctx, http.MethodPost, fmt.Sprintf("/v1/orgs/%s/%s", org, verb), token, nil)
}
