package agent

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// resolveFilePath returns p as an absolute, cleaned path, resolving
// relative paths against the process working directory.
func resolveFilePath(p string) (string, error) {
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(wd, p), nil
}

// fileLocks holds one mutex per absolute path, serializing GET/PUT (and
// future) operations on the same file so concurrent requests cannot
// interleave. Entries are never evicted — paths handled by this agent are
// expected to be bounded.
var fileLocks sync.Map // map[string]*sync.Mutex

// lockPath locks the per-path mutex for p and returns the unlock function.
func lockPath(p string) func() {
	m, _ := fileLocks.LoadOrStore(p, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// requirePath extracts the "path" query parameter, resolves it to an
// absolute path and locks it. On failure it writes the error response and
// returns ok == false.
func requirePath(w http.ResponseWriter, r *http.Request) (p string, unlock func(), ok bool) {
	raw := r.URL.Query().Get("path")
	if raw == "" {
		http.Error(w, "missing query parameter: path", http.StatusBadRequest)
		return "", nil, false
	}
	p, err := resolveFilePath(raw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return "", nil, false
	}
	return p, lockPath(p), true
}

// FilesGetHandler serves GET /v1/file?path=<file>: the file at path is
// returned as an attachment. path may be absolute, or relative to the
// process working directory.
func FilesGetHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, unlock, ok := requirePath(w, r)
		if !ok {
			return
		}
		defer unlock()
		f, err := os.Open(p)
		if err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "not found: "+p, http.StatusNotFound)
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if st.IsDir() {
			http.Error(w, "is a directory: "+p, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": st.Name()}))
		http.ServeContent(w, r, st.Name(), st.ModTime(), f)
	}
}

// FilesPutHandler serves PUT /v1/file?path=<file>: the request body is
// written to path atomically — the body lands in a temporary file in the
// same directory, which is then renamed over path, so concurrent readers
// never see a partial file. Missing parent directories are created
// automatically. When the url query parameter is given, the body is
// expected to be empty and the content is downloaded from that http(s)
// URL instead, with the same atomic write. Optional query parameters:
//
//	url   — download content from this http(s) URL instead of the body
//	chmod — file mode as zero-prefixed octal, e.g. "0644" (default "0644")
//	chown — owner as "user:group"; either side may be a name or a numeric
//	        id, and either side may be omitted ("user", ":group", "1000:1000")
func FilesPutHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		p := q.Get("path")
		if p == "" {
			http.Error(w, "missing query parameter: path", http.StatusBadRequest)
			return
		}
		// Validate options before touching the filesystem.
		mode := os.FileMode(0o644)
		if s := q.Get("chmod"); s != "" {
			m, err := parseChmod(s)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mode = m
		}
		uid, gid := -1, -1
		if s := q.Get("chown"); s != "" {
			u, g, err := parseChown(s)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			uid, gid = u, g
		}
		rawURL := q.Get("url")
		if rawURL != "" {
			if err := validatePutURL(rawURL); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		p, unlock, ok := requirePath(w, r)
		if !ok {
			return
		}
		defer unlock()
		// The content source is the request body, unless url is given.
		src, ok := putSource(w, r, rawURL)
		if !ok {
			return
		}
		defer src.Close()
		// Parent directories are created automatically, like mkdir -p.
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Write to a temp file in the same directory (same filesystem, so
		// the rename below is atomic), then rename it over the target.
		f, err := os.CreateTemp(filepath.Dir(p), ".ironhive-upload-*")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		tmpName := f.Name()
		// No-op after a successful rename; removes the temp file on any
		// failure path below.
		defer os.Remove(tmpName)
		if _, err := io.Copy(f, src); err != nil {
			f.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// chown first: it may clear setuid/setgid bits set by chmod.
		if uid >= 0 || gid >= 0 {
			if err := f.Chown(uid, gid); err != nil {
				f.Close()
				http.Error(w, "chown: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if err := f.Chmod(mode); err != nil {
			f.Close()
			http.Error(w, "chmod: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// Flush to disk before the rename, so a crash cannot leave the
		// target renamed to a file whose content was never persisted.
		if err := f.Sync(); err != nil {
			f.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := f.Close(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.Rename(tmpName, p); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("OK"))
	}
}

// validatePutURL checks that rawURL is an absolute http(s) URL.
func validatePutURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid url: must be an absolute http(s) URL")
	}
	return nil
}

// putSource returns the content source for a PUT request: the request
// body, or when rawURL is non-empty the body of a GET to that URL. On
// failure it writes the error response and returns ok == false. The
// caller must close the returned reader.
func putSource(w http.ResponseWriter, r *http.Request, rawURL string) (src io.ReadCloser, ok bool) {
	if rawURL == "" {
		return r.Body, true
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		http.Error(w, "download: "+err.Error(), http.StatusBadGateway)
		return nil, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "download: "+err.Error(), http.StatusBadGateway)
		return nil, false
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		http.Error(w, "download: "+resp.Status, http.StatusBadGateway)
		return nil, false
	}
	return resp.Body, true
}

// parseChmod parses a zero-prefixed octal file mode, e.g. "0644".
func parseChmod(s string) (os.FileMode, error) {
	if !strings.HasPrefix(s, "0") {
		return 0, fmt.Errorf("invalid chmod %q: must be zero-prefixed octal", s)
	}
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid chmod %q: %v", s, err)
	}
	return os.FileMode(n), nil
}

// parseChown parses "user:group" into numeric ids; either side may be a
// name or a numeric id, and may be omitted (-1 = leave unchanged).
func parseChown(s string) (uid, gid int, err error) {
	u, g, _ := strings.Cut(s, ":")
	if uid, err = resolveUserID(u); err != nil {
		return -1, -1, err
	}
	if gid, err = resolveGroupID(g); err != nil {
		return -1, -1, err
	}
	return uid, gid, nil
}

func resolveUserID(s string) (int, error) {
	if s == "" {
		return -1, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	u, err := user.Lookup(s)
	if err != nil {
		return -1, fmt.Errorf("invalid chown user %q: %v", s, err)
	}
	return strconv.Atoi(u.Uid)
}

func resolveGroupID(s string) (int, error) {
	if s == "" {
		return -1, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	g, err := user.LookupGroup(s)
	if err != nil {
		return -1, fmt.Errorf("invalid chown group %q: %v", s, err)
	}
	return strconv.Atoi(g.Gid)
}
