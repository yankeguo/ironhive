package ironhive

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sandbox is a handle to one allocated sandbox pod. Agent calls go
// through the controller's reverse proxy, addressed by the pod name in
// the X-Sandbox-ID header.
type Sandbox struct {
	client *Client
	// Name is the pod name, as returned by Allocate.
	Name string
	// LeaseExpires is the current lease deadline; Renew keeps it current.
	LeaseExpires time.Time
}

// Renew extends the lease to lease from now and updates LeaseExpires.
func (s *Sandbox) Renew(ctx context.Context, lease time.Duration) error {
	expires, err := s.client.Renew(ctx, s.Name, lease)
	if err != nil {
		return err
	}
	s.LeaseExpires = expires
	return nil
}

// Release destroys the sandbox. Always call it when done — the lease
// would reclaim the pod eventually, but releasing is immediate.
func (s *Sandbox) Release(ctx context.Context) error {
	return s.client.Release(ctx, s.Name)
}

// AgentDo is the low-level escape hatch: it issues any request against
// the agent inside the sandbox pod, path included (the agent serves its
// API under /agent/v1/...). The response body is open on success; the
// caller must close it.
func (s *Sandbox) AgentDo(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Response, error) {
	return s.client.do(ctx, method, path, query, body, http.Header{
		"X-Sandbox-ID": {s.Name},
	})
}

// PermOptions carries the optional chmod / chown of file and directory
// writes: Chmod is a zero-prefixed octal ("0644"), Chown is "user:group"
// (names or numeric ids, either side omittable).
type PermOptions struct {
	Chmod string
	Chown string
}

func (o *PermOptions) apply(q url.Values) {
	if o == nil {
		return
	}
	if o.Chmod != "" {
		q.Set("chmod", o.Chmod)
	}
	if o.Chown != "" {
		q.Set("chown", o.Chown)
	}
}

// TarOptions carries the repeatable include / exclude filters of the tar
// endpoints — path.Match syntax extended with "**", excludes win.
type TarOptions struct {
	Include []string
	Exclude []string
}

func (o *TarOptions) apply(q url.Values) {
	if o == nil {
		return
	}
	for _, p := range o.Include {
		q.Add("include", p)
	}
	for _, p := range o.Exclude {
		q.Add("exclude", p)
	}
}

// GetFile downloads the file at path (absolute, or relative to the
// agent's working directory). Close the returned reader when done.
func (s *Sandbox) GetFile(ctx context.Context, path string) (io.ReadCloser, error) {
	resp, err := s.AgentDo(ctx, http.MethodGet, "/agent/v1/file", url.Values{"path": {path}}, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// PutFile uploads r atomically to path; missing parent directories are
// created automatically.
func (s *Sandbox) PutFile(ctx context.Context, path string, r io.Reader, opts *PermOptions) error {
	q := url.Values{"path": {path}}
	opts.apply(q)
	resp, err := s.AgentDo(ctx, http.MethodPut, "/agent/v1/file", q, r)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// GetTar streams the directory at path as an uncompressed tar archive.
// Close the returned reader when done.
func (s *Sandbox) GetTar(ctx context.Context, path string, opts *TarOptions) (io.ReadCloser, error) {
	q := url.Values{"path": {path}}
	opts.apply(q)
	resp, err := s.AgentDo(ctx, http.MethodGet, "/agent/v1/tar", q, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// PutTar extracts an uncompressed tar stream into path.
func (s *Sandbox) PutTar(ctx context.Context, path string, r io.Reader, opts *TarOptions) error {
	q := url.Values{"path": {path}}
	opts.apply(q)
	resp, err := s.AgentDo(ctx, http.MethodPut, "/agent/v1/tar", q, r)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// UploadOptions carries the outgoing method (PUT / POST / PATCH, default
// POST) and extra headers ("key=value" entries) of the upload endpoints.
type UploadOptions struct {
	Method  string
	Headers []string
}

// UploadFile streams the local (in-sandbox) file at path to targetURL.
func (s *Sandbox) UploadFile(ctx context.Context, path, targetURL string, opts *UploadOptions) error {
	return s.upload(ctx, "/agent/v1/file/upload", path, targetURL, opts, nil)
}

// UploadTar packs the directory at path as a tar stream and uploads it to
// targetURL.
func (s *Sandbox) UploadTar(ctx context.Context, path, targetURL string, opts *UploadOptions, tar *TarOptions) error {
	return s.upload(ctx, "/agent/v1/tar/upload", path, targetURL, opts, tar)
}

func (s *Sandbox) upload(ctx context.Context, endpoint, path, targetURL string, opts *UploadOptions, tar *TarOptions) error {
	form := url.Values{"path": {path}, "url": {targetURL}}
	if opts != nil {
		if opts.Method != "" {
			form.Set("method", opts.Method)
		}
		for _, h := range opts.Headers {
			form.Add("headers", h)
		}
	}
	tar.apply(form)
	resp, err := s.AgentDo(ctx, http.MethodPost, endpoint, form, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// DirEntry is one entry of a directory listing.
type DirEntry struct {
	Name  string    `json:"name"`
	Dir   bool      `json:"dir"`
	Size  int64     `json:"size"`
	Mode  string    `json:"mode"` // zero-prefixed octal
	Mtime time.Time `json:"mtime"`
}

// ListDir lists the directory at path, sorted by name.
func (s *Sandbox) ListDir(ctx context.Context, path string) ([]DirEntry, error) {
	resp, err := s.AgentDo(ctx, http.MethodGet, "/agent/v1/dir", url.Values{"path": {path}}, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var entries []DirEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("ironhive: decode dir response: %w", err)
	}
	return entries, nil
}

// Mkdir creates the directory at path like mkdir -p (default mode 0755).
func (s *Sandbox) Mkdir(ctx context.Context, path string, opts *PermOptions) error {
	q := url.Values{"path": {path}}
	opts.apply(q)
	resp, err := s.AgentDo(ctx, http.MethodPut, "/agent/v1/dir", q, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ShellOptions carries the optional fields of a shell call: Cwd (the
// working directory), Env ("KEY=VALUE" entries) and StrictEnv (exactly
// the Env entries, without the agent's curated base environment).
type ShellOptions struct {
	Cwd       string
	Env       []string
	StrictEnv bool
}

// ShellEvent is one server-sent event of a shell call.
type ShellEvent struct {
	// Type is the SSE event name: stdout, stderr, exit, cwd or env.
	Type string
	// Data is the payload: the decoded text for stdout / stderr / exit /
	// cwd events, the raw JSON object for env.
	Data string
}

// Shell runs command via bash inside the sandbox and streams events to
// onEvent: one stdout / stderr event per output line, a final exit event
// with the exit code, then cwd / env snapshots. Cancelling ctx
// disconnects, which makes the agent SIGTERM the command's whole process
// group — the reliable cancel mechanism.
func (s *Sandbox) Shell(ctx context.Context, command string, opts *ShellOptions, onEvent func(ShellEvent) error) error {
	form := url.Values{"command": {command}}
	if opts != nil {
		if opts.Cwd != "" {
			form.Set("cwd", opts.Cwd)
		}
		for _, e := range opts.Env {
			form.Add("env", e)
		}
		if opts.StrictEnv {
			form.Set("strict_env", "true")
		}
	}
	resp, err := s.AgentDo(ctx, http.MethodPost, "/agent/v1/shell", form, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var eventType, data string
	dispatch := func() error {
		if eventType == "" {
			return nil
		}
		ev := ShellEvent{Type: eventType, Data: data}
		// stdout / stderr / exit / cwd carry a JSON-encoded string;
		// env carries a JSON object, left raw.
		var decoded string
		if err := json.Unmarshal([]byte(data), &decoded); err == nil {
			ev.Data = decoded
		}
		return onEvent(ev)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := dispatch(); err != nil {
				return err
			}
			eventType, data = "", ""
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("ironhive: read shell stream: %w", err)
	}
	return dispatch()
}
