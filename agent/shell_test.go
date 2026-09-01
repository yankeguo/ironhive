package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type capturedEvent struct {
	event string
	data  json.RawMessage
}

// runShell POSTs command to the handler and parses the SSE stream.
func runShell(t *testing.T, command string) (int, []capturedEvent) {
	t.Helper()
	return postShell(t, url.Values{"command": {command}})
}

// postShell POSTs a form to the handler and parses the SSE stream.
func postShell(t *testing.T, form url.Values) (int, []capturedEvent) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/shell", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	ShellPostHandler(NewConfig()).ServeHTTP(rec, req)
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
			ev.data = json.RawMessage(strings.TrimPrefix(line, "data: "))
		case line == "":
			events = append(events, ev)
			ev = capturedEvent{}
		}
	}
	return events
}

// eventData returns the data of every occurrence of event, JSON-unmarshaled
// into out (string for line/code events, map for the env event, ...).
func eventData[T any](t *testing.T, events []capturedEvent, event string) []T {
	t.Helper()
	var out []T
	for _, ev := range events {
		if ev.event != event {
			continue
		}
		var v T
		if err := json.Unmarshal(ev.data, &v); err != nil {
			t.Fatalf("bad %s data %s: %v", event, ev.data, err)
		}
		out = append(out, v)
	}
	return out
}

func TestShellStdoutStderrExit(t *testing.T) {
	code, events := runShell(t, "echo out; echo err >&2")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	stdout := eventData[string](t, events, "stdout")
	if len(stdout) != 1 || stdout[0] != "out" {
		t.Fatalf("stdout events = %v", stdout)
	}
	stderr := eventData[string](t, events, "stderr")
	if len(stderr) != 1 || stderr[0] != "err" {
		t.Fatalf("stderr events = %v", stderr)
	}
	exit := eventData[string](t, events, "exit")
	if len(exit) != 1 || exit[0] != "0" {
		t.Fatalf("exit events = %v", exit)
	}
}

func TestShellContentType(t *testing.T) {
	form := url.Values{"command": {"true"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/shell", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	ShellPostHandler(NewConfig()).ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestShellExitCode(t *testing.T) {
	_, events := runShell(t, "exit 3")
	exit := eventData[string](t, events, "exit")
	if len(exit) != 1 || exit[0] != "3" {
		t.Fatalf("exit events = %v", exit)
	}
}

func TestShellExitCodeSignal(t *testing.T) {
	// A signal death is reported as 128+signal, per shell convention.
	_, events := runShell(t, "kill -TERM $$")
	exit := eventData[string](t, events, "exit")
	if len(exit) != 1 || exit[0] != "143" {
		t.Fatalf("exit events = %v, want 143", exit)
	}
}

func TestShellStateless(t *testing.T) {
	dir := t.TempDir()
	runShell(t, "cd "+dir+" && export IHR_TEST_MARK=bravo")
	_, events := runShell(t, "pwd; echo mark=$IHR_TEST_MARK")
	stdout := strings.Join(eventData[string](t, events, "stdout"), "\n")
	if strings.Contains(stdout, dir) {
		t.Fatalf("cwd must not persist across calls, stdout = %q", stdout)
	}
	if strings.Contains(stdout, "mark=bravo") {
		t.Fatalf("env must not persist across calls, stdout = %q", stdout)
	}
}

func TestShellCwdEnvFields(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, events := postShell(t, url.Values{
		"command": {"pwd; echo mark=$IHR_TEST_MARK"},
		"cwd":     {dir},
		"env":     {"IHR_TEST_MARK=bravo"},
	})
	stdout := eventData[string](t, events, "stdout")
	if len(stdout) != 2 || stdout[0] != dir || stdout[1] != "mark=bravo" {
		t.Fatalf("stdout events = %v", stdout)
	}
}

func TestShellStateReported(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, events := runShell(t, "cd "+dir+" && export IHR_TEST_MARK=bravo")
	cwd := eventData[string](t, events, "cwd")
	if len(cwd) != 1 || cwd[0] != dir {
		t.Fatalf("cwd events = %v, want %q", cwd, dir)
	}
	env := eventData[map[string]string](t, events, "env")
	if len(env) != 1 {
		t.Fatalf("env events = %d, want 1", len(env))
	}
	if env[0]["IHR_TEST_MARK"] != "bravo" {
		t.Fatalf("reported env missing IHR_TEST_MARK: %v", env[0])
	}
}

// TestShellStateThreading emulates the upstream harness composing a
// session: call without strict_env, then feed the reported cwd/env back
// into the next call with strict_env=true.
func TestShellStateThreading(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, events := runShell(t, "cd "+dir+" && export IHR_TEST_MARK=bravo")
	cwd := eventData[string](t, events, "cwd")
	env := eventData[map[string]string](t, events, "env")
	if len(cwd) != 1 || len(env) != 1 {
		t.Fatalf("state not reported: cwd = %v, env = %v", cwd, env)
	}
	form := url.Values{
		"command":    {"pwd; echo mark=$IHR_TEST_MARK; echo home=${HOME-unset}"},
		"cwd":        {cwd[0]},
		"strict_env": {"true"},
	}
	for k, v := range env[0] {
		form.Add("env", k+"="+v)
	}
	_, events = postShell(t, form)
	stdout := eventData[string](t, events, "stdout")
	if len(stdout) != 3 || stdout[0] != dir || stdout[1] != "mark=bravo" {
		t.Fatalf("threaded call stdout = %v", stdout)
	}
}

func TestShellStrictEnv(t *testing.T) {
	// strict_env=true: the environment is exactly the given entries, no
	// process variables leak in (HOME is not synthesized by bash).
	_, events := postShell(t, url.Values{
		"command":    {"echo foo=$IHR_TEST_FOO; echo home=${HOME-unset}"},
		"env":        {"IHR_TEST_FOO=bar"},
		"strict_env": {"true"},
	})
	stdout := eventData[string](t, events, "stdout")
	if len(stdout) != 2 || stdout[0] != "foo=bar" || stdout[1] != "home=unset" {
		t.Fatalf("stdout events = %v", stdout)
	}
	// Default (strict_env=false): curated process vars are inherited and
	// env entries override them.
	if os.Getenv("HOME") == "" {
		t.Skip("test process has no HOME")
	}
	_, events = postShell(t, url.Values{
		"command": {"echo home=${HOME-unset}; echo foo=${IHR_TEST_FOO-unset}"},
		"env":     {"IHR_TEST_FOO=bar"},
	})
	stdout = eventData[string](t, events, "stdout")
	if len(stdout) != 2 || stdout[0] != "home="+os.Getenv("HOME") || stdout[1] != "foo=bar" {
		t.Fatalf("stdout events = %v", stdout)
	}
}

func TestShellStrictEnvEmpty(t *testing.T) {
	// strict_env=true with no env entries: the command environment must
	// be empty — not the process environment, which cmd.Env == nil
	// would silently inherit.
	t.Setenv("IHR_LEAK_MARKER", "leaked")
	_, events := postShell(t, url.Values{
		"command":    {"echo marker=${IHR_LEAK_MARKER-unset}"},
		"strict_env": {"true"},
	})
	stdout := eventData[string](t, events, "stdout")
	if len(stdout) != 1 || stdout[0] != "marker=unset" {
		t.Fatalf("stdout events = %v, process environment leaked", stdout)
	}
	// The env event, reported from the command's own `env -0` snapshot,
	// must not contain the marker either.
	env := eventData[map[string]string](t, events, "env")
	if len(env) != 1 {
		t.Fatalf("env events = %d, want 1", len(env))
	}
	if _, ok := env[0]["IHR_LEAK_MARKER"]; ok {
		t.Fatalf("reported env contains IHR_LEAK_MARKER: %v", env[0])
	}
}

func TestShellOverlongLine(t *testing.T) {
	// A single output line beyond the scanner's 1MiB cap fails the
	// scanner; the handler must still drain the pipe, let the command
	// exit, and finish the stream with error and exit events.
	done := make(chan []capturedEvent, 1)
	go func() {
		_, events := runShell(t, `head -c 2000000 /dev/zero | tr '\0' 'a'; echo; echo AFTER`)
		done <- events
	}()
	var events []capturedEvent
	select {
	case events = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("request hung after an overlong output line")
	}
	if errs := eventData[string](t, events, "error"); len(errs) != 1 {
		t.Fatalf("error events = %v, want exactly 1", errs)
	}
	exit := eventData[string](t, events, "exit")
	if len(exit) != 1 || exit[0] != "0" {
		t.Fatalf("exit events = %v, want 0", exit)
	}
}

func TestCuratedEnv(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"HOME=/root",
		"LC_ALL=C.UTF-8",
		"KUBERNETES_SERVICE_HOST=10.0.0.1",
		"KUBERNETES_SERVICE_PORT=443",
		"MYAPP_SERVICE_HOST=10.0.0.2",
		"AWS_SECRET_ACCESS_KEY=secret",
		"HOSTNAME=pod-abc123",
	}
	got := curatedEnv(base, nil, false)
	want := []string{"PATH=/usr/bin", "HOME=/root", "LC_ALL=C.UTF-8"}
	if len(got) != len(want) {
		t.Fatalf("curatedEnv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("curatedEnv = %v, want %v", got, want)
		}
	}
}

func TestEnvAllowed(t *testing.T) {
	patterns := []string{"APP_*", "JAVA_OPTS"}

	// No override: built-in allowlist and LC_* apply (patterns is empty
	// then, but must be ignored anyway).
	for _, key := range []string{"PATH", "LC_ALL"} {
		if !envAllowed(key, patterns, false) {
			t.Fatalf("envAllowed(%q, _, false) = false, want true", key)
		}
	}
	for _, key := range []string{"KUBERNETES_SERVICE_HOST", "APP_HOME"} {
		if envAllowed(key, patterns, false) {
			t.Fatalf("envAllowed(%q, _, false) = true, want false", key)
		}
	}

	// Override: patterns are the complete allowlist.
	for _, key := range []string{"APP_HOME", "APP_", "JAVA_OPTS"} {
		if !envAllowed(key, patterns, true) {
			t.Fatalf("envAllowed(%q, patterns, true) = false, want true", key)
		}
	}
	for _, key := range []string{"PATH", "LC_ALL", "APP", "JAVA_OPTSX", "AWS_SECRET_ACCESS_KEY"} {
		if envAllowed(key, patterns, true) {
			t.Fatalf("envAllowed(%q, patterns, true) = true, want false", key)
		}
	}
}

func TestShellParallel(t *testing.T) {
	call := func() int {
		form := url.Values{"command": {"sleep 1"}}
		req := httptest.NewRequest(http.MethodPost, "/v1/shell", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		ShellPostHandler(NewConfig()).ServeHTTP(rec, req)
		return rec.Code
	}
	start := time.Now()
	codes := make(chan int, 2)
	go func() { codes <- call() }()
	go func() { codes <- call() }()
	for i := 0; i < 2; i++ {
		if code := <-codes; code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
	}
	// Two `sleep 1` calls must overlap; serial execution takes 2s+.
	if d := time.Since(start); d > 1800*time.Millisecond {
		t.Fatalf("two sleep 1 calls took %v; shell calls not parallel", d)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("/tmp/ironhive-shell-123/pwd"); got != "'/tmp/ironhive-shell-123/pwd'" {
		t.Fatalf("shellQuote = %q", got)
	}
	if got := shellQuote("/tmp/a'b"); got != `'/tmp/a'\''b'` {
		t.Fatalf("shellQuote with quote = %q", got)
	}
}

// TestCheckBash: the startup probe accepts a real bash and fails fast when
// none is on PATH — the lookup follows the process PATH, same as the
// handler's spawn.
func TestCheckBash(t *testing.T) {
	if err := CheckBash(); err != nil {
		t.Fatalf("CheckBash with bash on PATH: %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if err := CheckBash(); err == nil {
		t.Fatal("CheckBash without bash on PATH: want error, got nil")
	}
}

// TestShellQueryParams: POST endpoints accept parameters in the query
// string as well as the form body.
func TestShellQueryParams(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost,
		"/v1/shell?command="+url.QueryEscape("echo hi"), nil)
	rec := httptest.NewRecorder()
	ShellPostHandler(NewConfig()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	stdout := eventData[string](t, parseSSE(t, rec.Body.String()), "stdout")
	if len(stdout) != 1 || stdout[0] != "hi" {
		t.Fatalf("stdout events = %v", stdout)
	}
}

func TestShellBadParams(t *testing.T) {
	for name, form := range map[string]url.Values{
		"missing command": {},
		"env without =":   {"command": {"true"}, "env": {"NOVALUE"}},
		"env empty key":   {"command": {"true"}, "env": {"=value"}},
		"cwd missing":     {"command": {"true"}, "cwd": {filepath.Join(t.TempDir(), "nope")}},
		"cwd is a file":   {"command": {"true"}, "cwd": {"/dev/null"}},
		"strict_env junk": {"command": {"true"}, "strict_env": {"yes-please"}},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/shell", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			ShellPostHandler(NewConfig()).ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
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
		ShellPostHandler(NewConfig()).ServeHTTP(rec, req)
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
	// The command died to the SIGTERM teardown: 128+15.
	exit := eventData[string](t, parseSSE(t, rec.Body.String()), "exit")
	if len(exit) != 1 || exit[0] != "143" {
		t.Fatalf("exit events = %v, want 143", exit)
	}
}
