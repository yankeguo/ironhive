package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type capturedEvent struct{ event, data string }

// runShell POSTs command to the handler and parses the SSE stream.
func runShell(t *testing.T, command string) (int, []capturedEvent) {
	t.Helper()
	form := url.Values{"command": {command}}
	req := httptest.NewRequest(http.MethodPost, "/v1/shell", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	ShellPostHandler().ServeHTTP(rec, req)
	return rec.Code, parseSSE(t, rec.Body.String())
}

// parseSSE parses an SSE stream body into events.
func parseSSE(t *testing.T, body string) []capturedEvent {
	t.Helper()
	var events []capturedEvent
	sc := bufio.NewScanner(strings.NewReader(body))
	var ev capturedEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			ev.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev.data); err != nil {
				t.Fatalf("bad data line %q: %v", line, err)
			}
		case line == "":
			events = append(events, ev)
			ev = capturedEvent{}
		}
	}
	return events
}

func eventData(events []capturedEvent, event string) []string {
	var out []string
	for _, ev := range events {
		if ev.event == event {
			out = append(out, ev.data)
		}
	}
	return out
}

func TestShellStdoutStderrExit(t *testing.T) {
	form := url.Values{"command": {"echo out; echo err >&2"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/shell", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	ShellPostHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	code, events := rec.Code, parseSSE(t, rec.Body.String())
	_ = code
	stdout := eventData(events, "stdout")
	if len(stdout) != 1 || stdout[0] != "out" {
		t.Fatalf("stdout events = %v", stdout)
	}
	stderr := eventData(events, "stderr")
	if len(stderr) != 1 || stderr[0] != "err" {
		t.Fatalf("stderr events = %v", stderr)
	}
	exit := eventData(events, "exit")
	if len(exit) != 1 || exit[0] != "0" {
		t.Fatalf("exit events = %v", exit)
	}
}

func TestShellExitCode(t *testing.T) {
	_, events := runShell(t, "exit 3")
	exit := eventData(events, "exit")
	if len(exit) != 1 || exit[0] != "3" {
		t.Fatalf("exit events = %v", exit)
	}
}

func TestShellStatePersistence(t *testing.T) {
	dir := t.TempDir()
	runShell(t, "cd "+dir+" && export IHR_TEST_MARK=bravo")
	_, events := runShell(t, "pwd; echo mark=$IHR_TEST_MARK")
	stdout := strings.Join(eventData(events, "stdout"), "\n")
	if !strings.Contains(stdout, dir) {
		t.Fatalf("cwd not persisted, stdout = %q", stdout)
	}
	if !strings.Contains(stdout, "mark=bravo") {
		t.Fatalf("env not persisted, stdout = %q", stdout)
	}
}

func TestShellMissingCommand(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/shell", nil)
	rec := httptest.NewRecorder()
	ShellPostHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestShellCancelTerminatesCommand(t *testing.T) {
	form := url.Values{"command": {"sleep 60"}}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/shell", strings.NewReader(form.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		ShellPostHandler().ServeHTTP(rec, req)
		close(done)
	}()
	// Let the command start, then cancel the request; the handler should
	// tear the command down and return promptly.
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after request cancel")
	}
}
