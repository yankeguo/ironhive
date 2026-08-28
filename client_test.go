package ironhive

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"sandbox":      r.Form.Get("sandbox"),
			"leaseExpires": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("POST /controller/v1/release", func(w http.ResponseWriter, r *http.Request) {
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
				Ready: true, IP: "10.0.0.7", Allocated: true,
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
	mux.HandleFunc("GET /agent/v1/dir", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]DirEntry{
			{Name: "a.txt", Size: 5, Mode: "0644", Mtime: time.Now().UTC().Truncate(time.Second)},
			{Name: "sub", Dir: true, Mode: "0755", Mtime: time.Now().UTC().Truncate(time.Second)},
		})
	})
	mux.HandleFunc("POST /agent/v1/shell", func(w http.ResponseWriter, r *http.Request) {
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
	if len(state.Pods) != 1 || state.Pods[0].Name != "sandbox-01m13test" {
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
