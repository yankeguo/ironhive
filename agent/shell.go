package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"sigs.k8s.io/yaml"
)

// envAllowlist is the default set of process environment variables
// inherited by shell commands when strict_env is false. Only generic,
// portable vars are kept; platform-injected vars (Kubernetes
// `*_SERVICE_HOST` / `*_SERVICE_PORT`, cloud credentials, pod metadata,
// ...) are dropped so sandboxed commands cannot observe their environment.
// LC_* locale vars are kept by prefix. An image can replace this list
// entirely via agentConfigFile, see envAllowed.
var envAllowlist = map[string]bool{
	"PATH":    true,
	"HOME":    true,
	"USER":    true,
	"LOGNAME": true,
	"SHELL":   true,
	"LANG":    true,
	"TERM":    true,
	"TZ":      true,
	"TMPDIR":  true,
}

// agentConfigFile is an optional image-provided YAML config customizing
// the injected agent's behavior. It lets an image decide which of its own
// vars (e.g. APP_HOME, JAVA_OPTS) reach sandboxed commands without an
// agent rebuild.
const agentConfigFile = "/etc/ironhive/agent.yml"

// agentConfig mirrors agentConfigFile. AllowedEnvs is a pointer so a
// missing field (fall back to envAllowlist) is distinguishable from an
// explicitly empty list (pass nothing through).
type agentConfig struct {
	// allowed_envs — full replacement for envAllowlist: wildcard patterns
	// (`*` / `?` / `[...]` as in path.Match) naming the process
	// environment variables shell commands may inherit when strict_env is
	// false.
	AllowedEnvs *[]string `json:"allowed_envs"`
}

// configuredEnvPatterns lazily loads agentConfigFile once. The file is
// part of the image, so it cannot change under a running agent. The bool
// reports whether allowed_envs was set and envAllowlist is replaced.
var configuredEnvPatterns = sync.OnceValues(func() ([]string, bool) {
	patterns, override := loadEnvPatterns(agentConfigFile)
	if override {
		log.Printf("agent: env allowlist replaced by %d patterns from %s", len(patterns), agentConfigFile)
	}
	return patterns, override
})

// loadEnvPatterns parses the allowed_envs field of an agent config file.
// A missing, unreadable, or unparsable file, or a missing field, means no
// override: the caller falls back to the built-in allowlist.
func loadEnvPatterns(path string) (patterns []string, override bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var cfg agentConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Printf("agent: ignoring unparsable config file %s: %v", path, err)
		return nil, false
	}
	if cfg.AllowedEnvs == nil {
		return nil, false
	}
	for _, p := range *cfg.AllowedEnvs {
		// Validate eagerly so a typo surfaces in the log instead of
		// silently never matching.
		if _, err := pathpkg.Match(p, ""); err != nil {
			log.Printf("agent: ignoring invalid env pattern %q in %s: %v", p, path, err)
			continue
		}
		patterns = append(patterns, p)
	}
	return patterns, true
}

// envAllowed reports whether the process environment variable key may be
// inherited by shell commands. With override, patterns are the complete
// allowlist; otherwise the curated allowlist and the LC_* prefix apply.
// Env keys never contain '/', so path.Match wildcards behave intuitively.
func envAllowed(key string, patterns []string, override bool) bool {
	if !override {
		return envAllowlist[key] || strings.HasPrefix(key, "LC_")
	}
	for _, p := range patterns {
		if ok, _ := pathpkg.Match(p, key); ok {
			return true
		}
	}
	return false
}

// curatedEnv filters base down to the allowlisted generic vars, or to the
// configured patterns when agentConfigFile replaces the allowlist.
func curatedEnv(base []string) []string {
	patterns, override := configuredEnvPatterns()
	var out []string
	for _, e := range base {
		k, _, _ := strings.Cut(e, "=")
		if envAllowed(k, patterns, override) {
			out = append(out, e)
		}
	}
	return out
}

// buildShellWrapper wraps command with an EXIT trap that snapshots the
// final pwd and exported env into envPath/pwdPath, which the handler
// reports back as SSE events. The trap runs on normal exit and on SIGTERM
// cancel, but not when WaitDelay escalates to SIGKILL — in that case no
// state is reported. The paths are agent-generated (os.MkdirTemp) and
// embedded via shellQuote — adjacent single-quoted segments concatenate,
// so nothing in the trap argument is ever expanded, at definition or at
// run time; no shell variable or env entry carries them, so the command
// cannot see or tamper with them.
func buildShellWrapper(command, envPath, pwdPath string) string {
	return "trap 'pwd > " + shellQuote(pwdPath) + " 2>/dev/null; env -0 > " + shellQuote(envPath) + ` 2>/dev/null' EXIT
` + command
}

// shellQuote renders s as a single-quoted shell literal, safe to embed in
// bash code.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellPostHandler serves POST /agent/v1/shell, running the "command" parameter
// via bash. Parameters may arrive in the query string, the urlencoded form
// body, or both (body wins on conflicts). The shell is stateless: every
// call starts from the process working directory and environment, unless
// overridden by parameters:
//
//	cwd — working directory; absolute, or relative to the process working
//	      directory. Must be an existing directory.
//	env — repeatable KEY=VALUE entries for the command environment.
//	strict_env — when true, the command environment is exactly the env
//	      entries given; when false (default), they overlay a curated
//	      subset of the process environment (see envAllowlist). The
//	      intended loop: call without strict_env, read back the env
//	      event, then pass it all back with strict_env=true.
//
// Output streams back as server-sent events:
//
//	event: stdout / stderr — data: <json string, one per output line>
//	event: exit            — data: <json string, exit code; 128+signal when
//	      the command died to a signal, e.g. 143 for SIGTERM; -1 when the
//	      code could not be determined, e.g. WaitDelay escalation>
//	event: cwd             — data: <json string, working directory after exit>
//	event: env             — data: <json object, full environment after exit>
//	event: error           — data: <json string> (spawn or stream failures)
//
// The cwd/env events let the upstream harness thread state across calls:
// it can feed the reported values back as the cwd/env fields of the next
// call, composing sessions itself. They are absent when the command was
// SIGKILLed before its EXIT trap ran. Calls are fully independent and run
// concurrently.
//
// If the client disconnects, the command's process group is terminated
// with SIGTERM (so the EXIT trap still runs) and SIGKILLed after a grace
// period.
func ShellPostHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q, ok := formParams(w, r)
		if !ok {
			return
		}
		command := q.Get("command")
		if command == "" {
			writeError(w, "missing form field: command", http.StatusBadRequest)
			return
		}
		cwd := q.Get("cwd")
		if cwd != "" {
			if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
				writeError(w, "invalid cwd: not an existing directory: "+cwd, http.StatusBadRequest)
				return
			}
		}
		var env []string
		for _, e := range q["env"] {
			k, _, found := strings.Cut(e, "=")
			if !found || k == "" {
				writeError(w, fmt.Sprintf("invalid env entry %q: must be KEY=VALUE", e), http.StatusBadRequest)
				return
			}
			env = append(env, e)
		}
		strictEnv := false
		if s := q.Get("strict_env"); s != "" {
			b, err := strconv.ParseBool(s)
			if err != nil {
				writeError(w, "invalid strict_env: must be a boolean", http.StatusBadRequest)
				return
			}
			strictEnv = b
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")

		// Per-call snapshot files: shells run concurrently, so nothing may
		// be shared between calls.
		stateDir, err := os.MkdirTemp("", "ironhive-shell-")
		if err != nil {
			writeError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer os.RemoveAll(stateDir)
		envPath := filepath.Join(stateDir, "env")
		pwdPath := filepath.Join(stateDir, "pwd")

		cmd := exec.CommandContext(r.Context(), "bash", "-c", buildShellWrapper(command, envPath, pwdPath))
		cmd.Dir = cwd // empty means the process working directory
		// Run bash in its own process group so a cancel can SIGTERM the
		// whole tree (pipelines, subshells, `sleep`s), not just the bash
		// wrapper. SIGTERM (not the default SIGKILL) lets bash run the
		// EXIT trap that writes the pwd/env snapshot.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			if err == syscall.ESRCH {
				// The group is already gone; nothing to cancel.
				return nil
			}
			return err
		}
		// After a cancel, kill the process if it ignores SIGTERM (e.g.
		// `trap '' TERM`) for too long. Grandchildren ignoring SIGTERM
		// outlive this, but are reparented to PID 1 and reaped when they
		// eventually die.
		cmd.WaitDelay = 5 * time.Second
		base := []string(nil)
		if !strictEnv {
			base = curatedEnv(os.Environ())
		}
		cmd.Env = applyEnvOverrides(base, env)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			writeError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			writeError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := shellChildren.start(cmd); err != nil {
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
			// Wait runs concurrently with the readers on purpose: it
			// closes the parent pipe ends as soon as the process exits,
			// which unblocks the readers even when a backgrounded
			// grandchild keeps its inherited pipe fds open. Output still
			// in flight at that moment may be lost — the alternative
			// (read-then-Wait) hangs forever on such grandchildren.
			err := shellChildren.wait(cmd)
			wg.Wait()
			events <- sseEvent{"exit", strconv.Itoa(exitCode(err))}
			close(events)
		}()
		for ev := range events {
			writeSSE(w, flusher, ev.event, ev.data)
		}

		// Report the post-command state, if the EXIT trap managed to
		// snapshot it.
		if data, err := os.ReadFile(pwdPath); err == nil {
			writeSSE(w, flusher, "cwd", strings.TrimRight(string(data), "\n"))
		}
		if data, err := os.ReadFile(envPath); err == nil {
			writeSSE(w, flusher, "env", parseEnvDump(data))
		}
	}
}

// applyEnvOverrides returns base with each KEY=VALUE override applied,
// replacing existing entries with the same key.
func applyEnvOverrides(base, overrides []string) []string {
	for _, o := range overrides {
		k, _, _ := strings.Cut(o, "=")
		prefix := k + "="
		found := false
		for i, e := range base {
			if strings.HasPrefix(e, prefix) {
				base[i] = o
				found = true
			}
		}
		if !found {
			base = append(base, o)
		}
	}
	return base
}

// parseEnvDump parses an `env -0` snapshot into a map.
func parseEnvDump(data []byte) map[string]string {
	env := map[string]string{}
	for _, entry := range strings.Split(string(data), "\x00") {
		if k, v, ok := strings.Cut(entry, "="); ok {
			env[k] = v
		}
	}
	return env
}

type sseEvent struct {
	event string
	data  any
}

// scanSSE forwards lines from rd as SSE events until EOF; scanner
// failures (e.g. an overlong line) surface as error events, except the
// pipe closures expected when WaitDelay or a cancel tears things down.
func scanSSE(rd io.Reader, event string, ch chan<- sseEvent, wg *sync.WaitGroup) {
	defer wg.Done()
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		ch <- sseEvent{event, sc.Text()}
	}
	if err := sc.Err(); err != nil &&
		!errors.Is(err, io.ErrClosedPipe) &&
		!errors.Is(err, fs.ErrClosed) {
		ch <- sseEvent{"error", fmt.Sprintf("%s stream: %v", event, err)}
	}
}

// exitCode extracts the process exit code from a cmd.Wait error, mapping
// signal deaths to the shell convention 128+signal (e.g. 143 for SIGTERM),
// so the harness can tell a killed command apart from a plain failure.
// It returns -1 when no code is available, e.g. when WaitDelay escalated.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return -1
}

// writeSSE writes one event; data is JSON-encoded so arbitrary output
// (newlines, quotes, binary-ish bytes) survives the SSE framing.
func writeSSE(w io.Writer, f http.Flusher, event string, data any) {
	payload, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
	f.Flush()
}
