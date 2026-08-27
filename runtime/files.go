package runtime

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

// FilesGetHandler serves GET /v1/file?path=<file>: the file at path is
// returned as an attachment. path may be absolute, or relative to the
// process working directory.
func FilesGetHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		if p == "" {
			http.Error(w, "missing query parameter: path", http.StatusBadRequest)
			return
		}
		if !filepath.IsAbs(p) {
			wd, err := os.Getwd()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			p = filepath.Join(wd, p)
		}
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
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", st.Name()))
		http.ServeContent(w, r, st.Name(), st.ModTime(), f)
	}
}
