package agent

import (
	"encoding/json"
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

// writeMessage answers with status code and a JSON body of the form
// {"message": msg}. All non-data responses (successes and errors) use
// this envelope so fields can be added later without breaking clients.
func writeMessage(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

// writeError is the JSON drop-in replacement for http.Error.
func writeError(w http.ResponseWriter, msg string, code int) {
	writeMessage(w, code, msg)
}

// formParams parses and returns the merged query and urlencoded form-body
// parameters of a POST request — both are allowed, and body entries take
// precedence over query entries on conflicts. PUT endpoints must not use
// this: their parameters live only in the query string and the body is the
// data stream. On failure it writes the error response and returns
// ok == false.
func formParams(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	if err := r.ParseForm(); err != nil {
		writeError(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return r.Form, true
}

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

// requirePath extracts the "path" parameter from q (the query string for
// GET/PUT endpoints, the merged query+form for POST endpoints), resolves
// it to an absolute path and locks it. On failure it writes the error
// response and returns ok == false.
func requirePath(w http.ResponseWriter, q url.Values) (p string, unlock func(), ok bool) {
	raw := q.Get("path")
	if raw == "" {
		writeError(w, "missing parameter: path", http.StatusBadRequest)
		return "", nil, false
	}
	p, err := resolveFilePath(raw)
	if err != nil {
		writeError(w, err.Error(), http.StatusInternalServerError)
		return "", nil, false
	}
	return p, lockPath(p), true
}

// FilesGetHandler serves GET /v1/file?path=<file>: the file at path is
// returned as an attachment. path may be absolute, or relative to the
// process working directory.
func FilesGetHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, unlock, ok := requirePath(w, r.URL.Query())
		if !ok {
			return
		}
		defer unlock()
		f, err := os.Open(p)
		if err != nil {
			if os.IsNotExist(err) {
				writeError(w, "not found: "+p, http.StatusNotFound)
			} else {
				writeError(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			writeError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if st.IsDir() {
			writeError(w, "is a directory: "+p, http.StatusBadRequest)
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
		p, unlock, ok := requirePath(w, r.URL.Query())
		if !ok {
			return
		}
		defer unlock()
		// Validate options before touching the filesystem.
		q := r.URL.Query()
		mode, uid, gid, ok := parsePermOptions(w, q, 0o644)
		if !ok {
			return
		}
		rawURL := q.Get("url")
		if rawURL != "" {
			if err := validatePutURL(rawURL); err != nil {
				writeError(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		// The content source is the request body, unless url is given.
		src, ok := putSource(w, r, rawURL)
		if !ok {
			return
		}
		defer src.Close()
		// Parent directories are created automatically, like mkdir -p.
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			writeError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Write to a temp file in the same directory (same filesystem, so
		// the rename below is atomic), then rename it over the target.
		f, err := os.CreateTemp(filepath.Dir(p), ".ironhive-upload-*")
		if err != nil {
			writeError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		tmpName := f.Name()
		// No-op after a successful rename; removes the temp file on any
		// failure path below.
		defer os.Remove(tmpName)
		if _, err := io.Copy(f, src); err != nil {
			f.Close()
			writeError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// chown first: it may clear setuid/setgid bits set by chmod.
		if uid >= 0 || gid >= 0 {
			if err := f.Chown(uid, gid); err != nil {
				f.Close()
				writeError(w, "chown: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if err := f.Chmod(mode); err != nil {
			f.Close()
			writeError(w, "chmod: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// Flush to disk before the rename, so a crash cannot leave the
		// target renamed to a file whose content was never persisted.
		if err := f.Sync(); err != nil {
			f.Close()
			writeError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := f.Close(); err != nil {
			writeError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.Rename(tmpName, p); err != nil {
			writeError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeMessage(w, http.StatusOK, "OK")
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
		writeError(w, "download: "+err.Error(), http.StatusBadGateway)
		return nil, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeError(w, "download: "+err.Error(), http.StatusBadGateway)
		return nil, false
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		writeError(w, "download: "+resp.Status, http.StatusBadGateway)
		return nil, false
	}
	return resp.Body, true
}

// parseUploadOptions validates the url, method and headers query
// parameters shared by the upload endpoints. On failure it writes the
// error response and returns ok == false.
func parseUploadOptions(w http.ResponseWriter, q url.Values) (method, rawURL string, hdrs [][2]string, ok bool) {
	rawURL = q.Get("url")
	if rawURL == "" {
		writeError(w, "missing query parameter: url", http.StatusBadRequest)
		return "", "", nil, false
	}
	if err := validatePutURL(rawURL); err != nil {
		writeError(w, err.Error(), http.StatusBadRequest)
		return "", "", nil, false
	}
	method = strings.ToUpper(q.Get("method"))
	if method == "" {
		method = http.MethodPost
	}
	switch method {
	case http.MethodPut, http.MethodPost, http.MethodPatch:
	default:
		writeError(w, "invalid method: must be PUT, POST or PATCH", http.StatusBadRequest)
		return "", "", nil, false
	}
	for _, h := range q["headers"] {
		k, v, found := strings.Cut(h, "=")
		if !found || k == "" {
			writeError(w, fmt.Sprintf("invalid headers entry %q: must be key=value", h), http.StatusBadRequest)
			return "", "", nil, false
		}
		hdrs = append(hdrs, [2]string{k, v})
	}
	return method, rawURL, hdrs, true
}

// uploadStream streams body to rawURL with method and the given extra
// headers, writing "OK" on a 2xx upstream response and a 502 otherwise.
// size is the body length, or negative when unknown (chunked encoding).
// contentType is sent as the Content-Type header when non-empty.
func uploadStream(w http.ResponseWriter, r *http.Request, method, rawURL string, hdrs [][2]string, contentType string, body io.Reader, size int64) bool {
	req, err := http.NewRequestWithContext(r.Context(), method, rawURL, body)
	if err != nil {
		writeError(w, "upload: "+err.Error(), http.StatusInternalServerError)
		return false
	}
	if size >= 0 {
		req.ContentLength = size
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for _, h := range hdrs {
		req.Header.Add(h[0], h[1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeError(w, "upload: "+err.Error(), http.StatusBadGateway)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Include a bounded slice of the upstream body for debugging.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := "upload: upstream " + resp.Status
		if s := strings.TrimSpace(string(snippet)); s != "" {
			msg += ": " + s
		}
		writeError(w, msg, http.StatusBadGateway)
		return false
	}
	writeMessage(w, http.StatusOK, "OK")
	return true
}

// FilesUploadHandler serves POST /v1/file/upload?path=<file>&url=<url>:
// the local file at path is streamed as the request body to url, using the
// method given by the method query parameter (default POST; only PUT, POST
// and PATCH are accepted, since the request carries a body). Extra headers
// may be attached with the repeatable headers query parameter, each in
// "key=value" form. A non-2xx upstream response is reported as 502.
func FilesUploadHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q, ok := formParams(w, r)
		if !ok {
			return
		}
		// Validate options before touching the filesystem.
		method, rawURL, hdrs, ok := parseUploadOptions(w, q)
		if !ok {
			return
		}
		p, unlock, ok := requirePath(w, q)
		if !ok {
			return
		}
		defer unlock()
		f, err := os.Open(p)
		if err != nil {
			if os.IsNotExist(err) {
				writeError(w, "not found: "+p, http.StatusNotFound)
			} else {
				writeError(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			writeError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if st.IsDir() {
			writeError(w, "is a directory: "+p, http.StatusBadRequest)
			return
		}
		uploadStream(w, r, method, rawURL, hdrs, "", f, st.Size())
	}
}

// parsePermOptions validates the chmod and chown query parameters shared by
// the PUT endpoints, defaulting the mode to defMode. On failure it writes
// the error response and returns ok == false.
func parsePermOptions(w http.ResponseWriter, q url.Values, defMode os.FileMode) (mode os.FileMode, uid, gid int, ok bool) {
	mode = defMode
	if s := q.Get("chmod"); s != "" {
		m, err := parseChmod(s)
		if err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return 0, 0, 0, false
		}
		mode = m
	}
	uid, gid = -1, -1
	if s := q.Get("chown"); s != "" {
		u, g, err := parseChown(s)
		if err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return 0, 0, 0, false
		}
		uid, gid = u, g
	}
	return mode, uid, gid, true
}

// parseChmod parses a zero-prefixed octal file mode, e.g. "0644". Values
// are capped at 07777 (permission bits plus setuid/setgid/sticky): wider
// octal values would spill into os.FileMode's non-permission flag bits.
func parseChmod(s string) (os.FileMode, error) {
	if !strings.HasPrefix(s, "0") {
		return 0, fmt.Errorf("invalid chmod %q: must be zero-prefixed octal", s)
	}
	n, err := strconv.ParseUint(s, 8, 12)
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
