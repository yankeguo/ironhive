package runtime

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
)

// shellMu serializes shell executions: all invocations share the on-disk
// pwd/env state, so concurrent commands would race on it.
var shellMu sync.Mutex

// shellState resolves the on-disk state file paths once per process. The
// wrapper script snapshots pwd and exported env into these files on exit
// and restores them before running the next command, giving cd/export
// persistence across calls without a long-lived shell.
var shellState = sync.OnceValues(func() (paths struct{ env, pwd string }, err error) {
	dir := filepath.Join(os.TempDir(), "ironhive-shell")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return paths, err
	}
	paths.env = filepath.Join(dir, "env")
	paths.pwd = filepath.Join(dir, "pwd")
	return paths, nil
})

// buildShellWrapper embeds command (verbatim, as bash code) into a wrapper
// that restores the previous call's env and cwd first, and snapshots them
// back on exit. State file paths travel via env vars, not string
// interpolation, to avoid path injection.
func buildShellWrapper(command string) string {
	return `trap 'pwd > "$IHR_SHELL_STATE_PWD" 2>/dev/null; env -0 > "$IHR_SHELL_STATE_ENV" 2>/dev/null' EXIT
if [ -f "$IHR_SHELL_STATE_ENV" ]; then
  while IFS= read -r -d '' __kv; do export "$__kv"; done < "$IHR_SHELL_STATE_ENV"
fi
if [ -f "$IHR_SHELL_STATE_PWD" ]; then
  cd "$(cat "$IHR_SHELL_STATE_PWD")" 2>/dev/null || true
fi
unset __kv
` + command
}

// ShellPostHandler serves POST /v1/shell with form field "command". The
// command runs via bash in a wrapper that carries cwd and exported env
// over from the previous call (initial cwd is the process working
// directory). Output streams back as server-sent events:
//
//	event: stdout / stderr — data: <json string, one per output line>
//	event: exit            — data: <exit code>
//	event: error           — data: <json string> (spawn failures)
func ShellPostHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		command := r.PostFormValue("command")
		if command == "" {
			http.Error(w, "missing form field: command", http.StatusBadRequest)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")

		shellMu.Lock()
		defer shellMu.Unlock()

		state, err := shellState()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cmd := exec.Command("bash", "-c", buildShellWrapper(command))
		cmd.Env = append(os.Environ(),
			"IHR_SHELL_STATE_ENV="+state.env,
			"IHR_SHELL_STATE_PWD="+state.pwd,
		)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := cmd.Start(); err != nil {
			writeSSE(w, flusher, "error", err.Error())
			writeSSE(w, flusher, "exit", "127")
			return
		}

		events := make(chan sseEvent, 64)
		var wg sync.WaitGroup
		wg.Add(2)
		go scanSSE(stdout, "stdout", events, &wg)
		go scanSSE(stderr, "stderr", events, &wg)
		go func() {
			wg.Wait()
			events <- sseEvent{"exit", strconv.Itoa(exitCode(cmd.Wait()))}
			close(events)
		}()
		for ev := range events {
			writeSSE(w, flusher, ev.event, ev.data)
		}
	}
}

type sseEvent struct{ event, data string }

// scanSSE forwards lines from rd as SSE events until EOF.
func scanSSE(rd io.Reader, event string, ch chan<- sseEvent, wg *sync.WaitGroup) {
	defer wg.Done()
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		ch <- sseEvent{event, sc.Text()}
	}
}

// exitCode extracts the process exit code from a cmd.Wait error.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// writeSSE writes one event; data is JSON-encoded so arbitrary output
// (newlines, quotes, binary-ish bytes) survives the SSE framing.
func writeSSE(w io.Writer, f http.Flusher, event, data string) {
	payload, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
	f.Flush()
}
