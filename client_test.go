package ironhive

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeController emulates the controller and, under /agent/, the agent
// inside a sandbox pod.
func fakeController(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /controller/v1/allocate", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			t.Errorf("allocate parameters not sent as a form body: query=%q content-type=%q",
				r.URL.RawQuery, r.Header.Get("Content-Type"))
		}
		if err := r.ParseForm(); err != nil {
			t.Error("allocate: parse form:", err)
		}
		if d, err := time.ParseDuration(r.Form.Get("lease")); err != nil || d <= 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "invalid lease"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"sandbox":      "sandbox-01m13test",
			"leaseExpires": time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("POST /controller/v1/renew", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("renew query = %q, want empty", r.URL.RawQuery)
		}
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"sandbox":      r.Form.Get("sandbox"),
			"leaseExpires": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("POST /controller/v1/release", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("release query = %q, want empty", r.URL.RawQuery)
		}
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"released": r.Form.Get("sandbox")})
	})
	mux.HandleFunc("GET /controller/v1/pools", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(PoolsState{
			Pools: []PoolSummary{{Name: "default", Standby: 2, Pending: 1, Allocated: 1}},
			Pods: []PodInfo{{
				Name: "sandbox-01m13test", Pool: "default", Phase: "Running",
				Ready: true, IP: "10.0.0.7", Deleting: true, Allocated: true,
				LeaseExpires: time.Now().Add(5 * time.Minute).UTC().Truncate(time.Second),
				CreatedAt:    time.Now().UTC().Truncate(time.Second),
			}},
		})
	})

	// Agent endpoints, reached through the proxy with X-Sandbox-ID.
	mux.HandleFunc("GET /agent/v1/file", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Sandbox-ID") == "" {
			t.Error("agent call without X-Sandbox-ID")
		}
		_, _ = w.Write([]byte("file-content"))
	})
	mux.HandleFunc("PUT /agent/v1/file", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "new-content" {
			t.Errorf("put file body = %q", body)
		}
		if r.URL.Query().Get("chmod") != "0600" {
			t.Errorf("chmod = %q", r.URL.Query().Get("chmod"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "OK"})
	})
	mux.HandleFunc("GET /agent/v1/tar", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query()["include"]; len(got) != 1 || got[0] != "**/*.txt" {
			t.Errorf("tar include = %v", got)
		}
		_, _ = io.WriteString(w, "tar-content")
	})
	mux.HandleFunc("PUT /agent/v1/tar", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "tar-body" {
			t.Errorf("put tar body = %q", body)
		}
		if got := r.URL.Query()["exclude"]; len(got) != 1 || got[0] != "*.tmp" {
			t.Errorf("tar exclude = %v", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "OK"})
	})
	mux.HandleFunc("PUT /agent/v1/dir", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") != "/tmp/new" || r.URL.Query().Get("chmod") != "0700" {
			t.Errorf("mkdir query = %v", r.URL.Query())
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "OK"})
	})
	upload := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("upload query = %q, want empty", r.URL.RawQuery)
		}
		if err := r.ParseForm(); err != nil {
			t.Error("upload: parse form:", err)
		}
		if r.Form.Get("path") == "" || r.Form.Get("url") == "" {
			t.Errorf("upload form = %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "OK"})
	}
	mux.HandleFunc("POST /agent/v1/file/upload", upload)
	mux.HandleFunc("POST /agent/v1/tar/upload", upload)
	mux.HandleFunc("GET /agent/v1/dir", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]DirEntry{
			{Name: "a.txt", Size: 5, Mode: "0644", Mtime: time.Now().UTC().Truncate(time.Second)},
			{Name: "sub", Dir: true, Mode: "0755", Mtime: time.Now().UTC().Truncate(time.Second)},
		})
	})
	mux.HandleFunc("POST /agent/v1/shell", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("shell query = %q, want empty", r.URL.RawQuery)
		}
		_ = r.ParseForm()
		if r.Form.Get("command") != "echo hi" {
			t.Errorf("shell command = %q", r.Form.Get("command"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: stdout\ndata: \"hi\"\n\n")
		_, _ = io.WriteString(w, "event: exit\ndata: \"0\"\n\n")
		_, _ = io.WriteString(w, "event: cwd\ndata: \"/app\"\n\n")
		_, _ = io.WriteString(w, "event: env\ndata: {\"HOME\":\"/root\"}\n\n")
	})

	return httptest.NewServer(mux)
}

func TestControllerEndpoints(t *testing.T) {
	srv := fakeController(t)
	defer srv.Close()
	c := NewClient(srv.URL)
	ctx := context.Background()

	sb, err := c.Allocate(ctx, "default", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Name != "sandbox-01m13test" {
		t.Fatalf("sandbox name = %q", sb.Name)
	}
	if sb.LeaseExpires.IsZero() {
		t.Fatal("lease deadline missing")
	}

	if err := sb.Renew(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if until := time.Until(sb.LeaseExpires); until < 59*time.Minute {
		t.Fatalf("renewed lease expires in %v, want ~1h", until)
	}

	if err := sb.Release(ctx); err != nil {
		t.Fatal(err)
	}

	state, err := c.Pools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pools) != 1 || state.Pools[0].Standby != 2 || state.Pools[0].Allocated != 1 {
		t.Fatalf("pools = %+v", state.Pools)
	}
	if len(state.Pods) != 1 || state.Pods[0].Name != "sandbox-01m13test" || !state.Pods[0].Deleting {
		t.Fatalf("pods = %+v", state.Pods)
	}
}

func TestErrorEnvelope(t *testing.T) {
	srv := fakeController(t)
	defer srv.Close()
	c := NewClient(srv.URL)

	// A zero lease is rejected by the stub with the message envelope.
	_, err := c.Allocate(context.Background(), "default", 0)
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("want *Error, got %v", err)
	}
	if e.StatusCode != http.StatusBadRequest || e.Message != "invalid lease" {
		t.Fatalf("error = %+v", e)
	}
}

func TestAgentPassthrough(t *testing.T) {
	srv := fakeController(t)
	defer srv.Close()
	c := NewClient(srv.URL)
	ctx := context.Background()

	sb, err := c.Allocate(ctx, "default", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	r, err := sb.GetFile(ctx, "/tmp/x")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r)
	r.Close()
	if string(data) != "file-content" {
		t.Fatalf("get file = %q", data)
	}

	if err := sb.PutFile(ctx, "/tmp/x", strings.NewReader("new-content"), &PermOptions{Chmod: "0600"}); err != nil {
		t.Fatal(err)
	}

	entries, err := sb.ListDir(ctx, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "a.txt" || !entries[1].Dir {
		t.Fatalf("entries = %+v", entries)
	}

	tarReader, err := sb.GetTar(ctx, "/tmp", &TarOptions{Include: []string{"**/*.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	tarData, _ := io.ReadAll(tarReader)
	tarReader.Close()
	if string(tarData) != "tar-content" {
		t.Fatalf("get tar = %q", tarData)
	}
	if err := sb.PutTar(ctx, "/tmp/out", strings.NewReader("tar-body"),
		&TarOptions{Exclude: []string{"*.tmp"}}); err != nil {
		t.Fatal(err)
	}
	if err := sb.Mkdir(ctx, "/tmp/new", &PermOptions{Chmod: "0700"}); err != nil {
		t.Fatal(err)
	}
	if err := sb.UploadFile(ctx, "/tmp/x", "https://example.test/file",
		&UploadOptions{Method: http.MethodPut, Headers: []string{"X-Test=yes"}}); err != nil {
		t.Fatal(err)
	}
	if err := sb.UploadTar(ctx, "/tmp", "https://example.test/tar", nil,
		&TarOptions{Include: []string{"**"}}); err != nil {
		t.Fatal(err)
	}
}

func TestShellEvents(t *testing.T) {
	srv := fakeController(t)
	defer srv.Close()
	c := NewClient(srv.URL)
	ctx := context.Background()

	sb, err := c.Allocate(ctx, "default", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	var events []ShellEvent
	err = sb.Shell(ctx, "echo hi", nil, func(ev ShellEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Type != "stdout" || events[0].Data != "hi" {
		t.Errorf("stdout event = %+v", events[0])
	}
	if events[1].Type != "exit" || events[1].Data != "0" {
		t.Errorf("exit event = %+v", events[1])
	}
	if events[2].Type != "cwd" || events[2].Data != "/app" {
		t.Errorf("cwd event = %+v", events[2])
	}
	if events[3].Type != "env" || !strings.Contains(events[3].Data, `"HOME":"/root"`) {
		t.Errorf("env event = %+v", events[3])
	}
}

func TestShellNilCallbackDrainsEvents(t *testing.T) {
	srv := fakeController(t)
	defer srv.Close()
	sb := &Sandbox{client: NewClient(srv.URL), Name: "sandbox-01m13test"}
	if err := sb.Shell(context.Background(), "echo hi", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPostFormsDoNotChangeAgentDoQuerySemantics(t *testing.T) {
	longCommand := strings.Repeat("x", 9000)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/v1/shell", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("shell query = %q, want empty", r.URL.RawQuery)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		if got := r.Form.Get("command"); got != longCommand {
			t.Errorf("command length = %d, want %d", len(got), len(longCommand))
		}
		_, _ = io.WriteString(w, "event: exit\ndata: \"0\"\n\n")
	})
	mux.HandleFunc("POST /agent/v1/custom", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("mode"); got != "query" {
			t.Errorf("custom mode = %q, want query", got)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "payload" {
			t.Errorf("custom body = %q", body)
		}
		_, _ = io.WriteString(w, "OK")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	sb := &Sandbox{client: NewClient(srv.URL), Name: "sandbox-test"}

	if err := sb.Shell(context.Background(), longCommand, nil, nil); err != nil {
		t.Fatal(err)
	}
	resp, err := sb.AgentDo(context.Background(), http.MethodPost, "/agent/v1/custom",
		url.Values{"mode": {"query"}}, strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestClientRejectsRedirectAsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	h := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	_, err := NewClient(srv.URL, WithHTTPClient(h)).Pools(context.Background())
	var responseErr *Error
	if !errors.As(err, &responseErr) || responseErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("error = %v, want *Error status 307", err)
	}
}

func TestInvalidClientAndSandboxReturnErrors(t *testing.T) {
	var nilClient *Client
	if _, err := nilClient.Pools(context.Background()); err == nil {
		t.Fatal("nil client did not return an error")
	}
	var zeroClient Client
	if _, err := zeroClient.Pools(context.Background()); err == nil {
		t.Fatal("zero client did not return an error")
	}
	var nilSandbox *Sandbox
	if err := nilSandbox.Release(context.Background()); err == nil {
		t.Fatal("nil sandbox did not return an error")
	}
	invalid := &Sandbox{Name: "sandbox-test"}
	if _, err := invalid.GetFile(context.Background(), "/tmp/x"); err == nil {
		t.Fatal("detached sandbox did not return an error")
	}
	if err := invalid.Shell(context.Background(), "true", nil, nil); err == nil {
		t.Fatal("detached sandbox shell did not return an error")
	}
}

func TestShellAcceptsJSONExpandedAgentLine(t *testing.T) {
	raw := strings.Repeat("\x00", 1<<20)
	payload, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) <= 4*1024*1024 || len(payload) >= shellSSEMaxLine {
		t.Fatalf("payload size = %d, want between old and new scanner limits", len(payload))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "event: stdout\ndata: ")
		_, _ = w.Write(payload)
		_, _ = io.WriteString(w, "\n\nevent: exit\ndata: \"0\"\n\n")
	}))
	defer srv.Close()
	sb := &Sandbox{client: NewClient(srv.URL), Name: "sandbox-test"}
	var got string
	err = sb.Shell(context.Background(), "ignored", nil, func(ev ShellEvent) error {
		if ev.Type == "stdout" {
			got = ev.Data
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != raw {
		t.Fatalf("stdout length = %d, want %d", len(got), len(raw))
	}
}

func TestRequestContextCancellation(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := NewClient(srv.URL).Pools(ctx)
		errCh <- err
	}()
	<-started
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request did not stop after context cancellation")
	}
}

func TestAgentErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "agent unavailable"})
	}))
	defer srv.Close()
	sb := &Sandbox{client: NewClient(srv.URL), Name: "sandbox-test"}
	_, err := sb.GetFile(context.Background(), "/tmp/x")
	var responseErr *Error
	if !errors.As(err, &responseErr) || responseErr.StatusCode != http.StatusBadGateway ||
		responseErr.Message != "agent unavailable" {
		t.Fatalf("error = %#v", err)
	}
}
