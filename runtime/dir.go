package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"syscall"
	"time"
)

// dirEntry is one item in a GET /v1/dir listing.
type dirEntry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	Mode  string `json:"mode"` // permission bits as zero-prefixed octal, e.g. "0644"
	Mtime string `json:"mtime"`
}

// DirGetHandler serves GET /v1/dir?path=<dir>: a JSON array of the
// directory's entries, sorted by name. path may be absolute, or relative
// to the process working directory.
func DirGetHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, unlock, ok := requirePath(w, r)
		if !ok {
			return
		}
		defer unlock()
		entries, err := os.ReadDir(p)
		if err != nil {
			switch {
			case os.IsNotExist(err):
				http.Error(w, "not found: "+p, http.StatusNotFound)
			case errors.Is(err, syscall.ENOTDIR):
				http.Error(w, "not a directory: "+p, http.StatusBadRequest)
			default:
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		out := make([]dirEntry, 0, len(entries))
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			out = append(out, dirEntry{
				Name:  e.Name(),
				Dir:   e.IsDir(),
				Size:  info.Size(),
				Mode:  fmt.Sprintf("%04o", info.Mode().Perm()),
				Mtime: info.ModTime().UTC().Format(time.RFC3339),
			})
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(out)
	}
}

// DirPutHandler serves PUT /v1/dir?path=<dir>: create the directory (and
// parents) like mkdir -p. Optional query parameters, same syntax as
// PUT /v1/file:
//
//	chmod — directory mode as zero-prefixed octal, e.g. "0755"
//	chown — owner as "user:group"; names or numeric ids, either side
//	        may be omitted
func DirPutHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		p := q.Get("path")
		if p == "" {
			http.Error(w, "missing query parameter: path", http.StatusBadRequest)
			return
		}
		mode := os.FileMode(0o755)
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
		p, unlock, ok := requirePath(w, r)
		if !ok {
			return
		}
		defer unlock()
		if err := os.MkdirAll(p, mode); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// MkdirAll only applies mode to newly created directories (and
		// umask interferes), so set ownership and mode explicitly; chown
		// first as it may clear mode bits.
		if uid >= 0 || gid >= 0 {
			if err := os.Chown(p, uid, gid); err != nil {
				http.Error(w, "chown: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if err := os.Chmod(p, mode); err != nil {
			http.Error(w, "chmod: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("OK"))
	}
}
