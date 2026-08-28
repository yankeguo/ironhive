// Package ironhive is a Go client for the ironhive controller and,
// through the controller's reverse proxy, for the agents running inside
// sandbox pods.
//
//	c := ironhive.NewClient("http://ironhive-controller:8080")
//	sb, err := c.Allocate(ctx, "default", 5*time.Minute)
//	if err != nil { ... }
//	defer sb.Release(context.Background())
//	err = sb.Shell(ctx, "echo hello", nil, func(ev ironhive.ShellEvent) error {
//		fmt.Println(ev.Type, ev.Data)
//		return nil
//	})
//
// Requests carry no client-side timeout, matching the controller/agent
// philosophy: deadlines and cancellation belong to the caller's context.
package ironhive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to an ironhive controller.
type Client struct {
	baseURL string
	http    *http.Client
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient sets the http.Client used for all requests — for custom
// transports, proxies or tracing. Do not set a Timeout on it; use
// per-call contexts instead, so long-running shell sessions keep working.
// A nil client is ignored.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// NewClient returns a Client for the controller at baseURL, e.g.
// "http://ironhive-controller:8080".
func NewClient(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    http.DefaultClient,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Error is a non-2xx response from the controller or an agent, decoded
// from the {"message": ...} envelope both use.
type Error struct {
	StatusCode int
	Message    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("ironhive: status %d: %s", e.StatusCode, e.Message)
}

// PoolSummary aggregates the pod counts of one pool.
type PoolSummary struct {
	Name      string `json:"name"`
	Standby   int    `json:"standby"`
	Pending   int    `json:"pending"`
	Allocated int    `json:"allocated"`
}

// PodInfo describes one managed pod, as reported by GET
// /controller/v1/pools.
type PodInfo struct {
	Name         string    `json:"name"`
	Pool         string    `json:"pool"`
	Phase        string    `json:"phase"`
	Ready        bool      `json:"ready"`
	IP           string    `json:"ip"`
	Allocated    bool      `json:"allocated"`
	LeaseExpires time.Time `json:"leaseExpires"`
	CreatedAt    time.Time `json:"createdAt"`
}

// PoolsState is the controller's cluster overview.
type PoolsState struct {
	Pools []PoolSummary `json:"pools"`
	Pods  []PodInfo     `json:"pods"`
}

// Allocate claims one Ready standby pod of the pool, leased for lease.
// The call blocks server-side (up to 30s) waiting for a pod to become
// available; pass a deadline in ctx to bound the total wait. The returned
// Sandbox is the handle for renew, release and all agent calls.
func (c *Client) Allocate(ctx context.Context, pool string, lease time.Duration) (*Sandbox, error) {
	form := url.Values{"pool": {pool}, "lease": {lease.String()}}
	resp, err := c.doPostForm(ctx, "/controller/v1/allocate", form, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Sandbox      string    `json:"sandbox"`
		LeaseExpires time.Time `json:"leaseExpires"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("ironhive: decode allocate response: %w", err)
	}
	return &Sandbox{client: c, Name: out.Sandbox, LeaseExpires: out.LeaseExpires}, nil
}

// Renew extends the lease of the named sandbox to lease from now and
// returns the new deadline.
func (c *Client) Renew(ctx context.Context, name string, lease time.Duration) (time.Time, error) {
	form := url.Values{"sandbox": {name}, "lease": {lease.String()}}
	resp, err := c.doPostForm(ctx, "/controller/v1/renew", form, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	var out struct {
		LeaseExpires time.Time `json:"leaseExpires"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return time.Time{}, fmt.Errorf("ironhive: decode renew response: %w", err)
	}
	return out.LeaseExpires, nil
}

// Release destroys the named sandbox; the pool is topped up with a fresh
// one asynchronously.
func (c *Client) Release(ctx context.Context, name string) error {
	form := url.Values{"sandbox": {name}}
	resp, err := c.doPostForm(ctx, "/controller/v1/release", form, nil)
	if err != nil {
		return err
	}
	closeBody(resp.Body)
	return nil
}

// Pools fetches the read-only cluster overview behind the dashboard.
func (c *Client) Pools(ctx context.Context) (*PoolsState, error) {
	resp, err := c.do(ctx, http.MethodGet, "/controller/v1/pools", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out PoolsState
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("ironhive: decode pools response: %w", err)
	}
	return &out, nil
}

// do issues one request and turns non-2xx responses into *Error. The
// response body is open on success; the caller must close it.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body io.Reader, header http.Header) (*http.Response, error) {
	if c == nil || c.http == nil {
		return nil, fmt.Errorf("ironhive: invalid client")
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range header {
		req.Header[k] = append([]string(nil), vs...)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, decodeError(resp)
	}
	return resp, nil
}

// doPostForm sends an internal POST request with its parameters in the
// urlencoded body. AgentDo deliberately remains query-preserving.
func (c *Client) doPostForm(ctx context.Context, path string, form url.Values, header http.Header) (*http.Response, error) {
	header = header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(ctx, http.MethodPost, path, nil, strings.NewReader(form.Encode()), header)
}

// decodeError reads the {"message": ...} envelope off a non-2xx response.
func decodeError(resp *http.Response) error {
	e := &Error{StatusCode: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
	var body struct {
		Message string `json:"message"`
	}
	// The envelope is small by convention; the cap keeps a misbehaving
	// server from making the client buffer an unbounded error body.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err == nil && body.Message != "" {
		e.Message = body.Message
	}
	return e
}

// closeBody drains the small envelope response so the underlying
// connection stays reusable, then closes the body.
func closeBody(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4096))
	body.Close()
}
