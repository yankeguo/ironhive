package agent

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// tarFilter decides which slash-separated, archive-relative names are
// packed or extracted. An empty include list matches everything; excludes
// win over includes, and excluding a directory excludes everything
// beneath it.
type tarFilter struct {
	include []string
	exclude []string
}

// newTarFilter reads the repeatable include/exclude query parameters.
// Patterns use path.Match syntax extended with "**", which matches zero
// or more path segments (crossing directories).
func newTarFilter(q url.Values) (*tarFilter, error) {
	f := &tarFilter{}
	for _, key := range []string{"include", "exclude"} {
		for _, pat := range q[key] {
			pat = strings.TrimSuffix(strings.TrimPrefix(pat, "./"), "/")
			for _, seg := range strings.Split(pat, "/") {
				if seg == "**" {
					continue
				}
				if _, err := path.Match(seg, ""); err != nil {
					return nil, fmt.Errorf("invalid %s pattern %q: %v", key, pat, err)
				}
			}
			if key == "include" {
				f.include = append(f.include, pat)
			} else {
				f.exclude = append(f.exclude, pat)
			}
		}
	}
	return f, nil
}

// excluded reports whether name or any of its ancestor directories
// matches an exclude pattern.
func (f *tarFilter) excluded(name string) bool {
	name = strings.TrimSuffix(strings.TrimPrefix(name, "./"), "/")
	for _, pat := range f.exclude {
		for n := name; ; {
			if matchGlob(pat, n) {
				return true
			}
			i := strings.LastIndex(n, "/")
			if i < 0 {
				break
			}
			n = n[:i]
		}
	}
	return false
}

// included reports whether name passes the filter.
func (f *tarFilter) included(name string) bool {
	if f.excluded(name) {
		return false
	}
	if len(f.include) == 0 {
		return true
	}
	name = strings.TrimSuffix(strings.TrimPrefix(name, "./"), "/")
	for _, pat := range f.include {
		if matchGlob(pat, name) {
			return true
		}
	}
	return false
}

// matchGlob reports whether the slash-separated name matches pattern,
// where a "**" segment matches zero or more name segments and every
// other segment follows path.Match syntax.
func matchGlob(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], name[0])
		if err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// TarGetHandler serves GET /v1/tar?path=<dir>: the directory at path is
// streamed back as an uncompressed tar archive attachment named
// "<dirname>.tar". Entry names are relative to the directory itself, so
// the archive round-trips through PUT /v1/tar into any destination.
// Directories and regular files are included with their modes and mtimes;
// symlinks and other special files are skipped. path may be absolute, or
// relative to the process working directory. The repeatable include and
// exclude query parameters limit which entries are archived: patterns use
// path.Match syntax extended with "**" (crossing directories), matched
// against archive-relative names; excludes win, and excluding a directory
// excludes its whole subtree.
func TarGetHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filter, err := newTarFilter(r.URL.Query())
		if err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		p, unlock, ok := requirePath(w, r.URL.Query())
		if !ok {
			return
		}
		defer unlock()
		st, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				writeError(w, "not found: "+p, http.StatusNotFound)
			} else {
				writeError(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		if !st.IsDir() {
			writeError(w, "not a directory: "+p, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment",
			map[string]string{"filename": st.Name() + ".tar"}))
		// The status is already sent, so a mid-stream failure can only
		// truncate the response; log it for server-side visibility.
		tw := tar.NewWriter(w)
		if err := writeDirToTar(tw, p, filter); err != nil {
			log.Printf("GET /v1/tar: %s: stream aborted: %v", p, err)
		}
		_ = tw.Close()
	}
}

// writeDirToTar walks root and writes its contents as tar entries named
// relative to root, skipping entries rejected by filter. Excluded
// directories are not descended into; directories not matching a
// non-empty include list are still descended into, so matching files
// beneath them are archived.
func writeDirToTar(tw *tar.Writer, root string, filter *tarFilter) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if filter.excluded(rel) {
				return filepath.SkipDir
			}
			if !filter.included(rel) {
				return nil
			}
		} else if !filter.included(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     rel,
			Mode:     int64(info.Mode().Perm()),
			Size:     info.Size(),
			ModTime:  info.ModTime(),
		}
		switch {
		case d.IsDir():
			hdr.Typeflag = tar.TypeDir
			hdr.Name = rel + "/"
			hdr.Size = 0
			return tw.WriteHeader(hdr)
		case info.Mode().IsRegular():
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tw, f)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		default:
			// Skip symlinks, devices, sockets, ...
			return nil
		}
	})
}

// TarUploadHandler serves POST /v1/tar/upload?path=<dir>&url=<url>: the
// directory at path is packed as an uncompressed tar stream — the same
// archive GET /v1/tar produces, honoring the same repeatable include and
// exclude filters — and streamed as the request body to url with
// Content-Type application/x-tar. method and headers behave as in
// POST /v1/file/upload. The stream length is unknown upfront, so the
// upload uses chunked encoding. A non-2xx upstream response is reported
// as 502.
func TarUploadHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q, ok := formParams(w, r)
		if !ok {
			return
		}
		method, rawURL, hdrs, ok := parseUploadOptions(w, q)
		if !ok {
			return
		}
		filter, err := newTarFilter(q)
		if err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		p, unlock, ok := requirePath(w, q)
		if !ok {
			return
		}
		defer unlock()
		st, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				writeError(w, "not found: "+p, http.StatusNotFound)
			} else {
				writeError(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		if !st.IsDir() {
			writeError(w, "not a directory: "+p, http.StatusBadRequest)
			return
		}
		// Pack and upload concurrently: the goroutine writes the archive
		// into the pipe while the HTTP client reads it as the request body.
		pr, pw := io.Pipe()
		go func() {
			tw := tar.NewWriter(pw)
			err := writeDirToTar(tw, p, filter)
			if cerr := tw.Close(); err == nil {
				err = cerr
			}
			_ = pw.CloseWithError(err)
		}()
		uploadStream(w, r, method, rawURL, hdrs, "application/x-tar", pr, -1)
		pr.Close()
	}
}

// errBadTar marks errors caused by the archive itself (corrupt stream,
// malicious entry names, unsupported entry types) as opposed to local
// filesystem failures, so the handler can answer 400 instead of 500.
var errBadTar = errors.New("invalid tar archive")

// TarPutHandler serves PUT /v1/tar?path=<dir>: the request body is an
// uncompressed tar stream extracted into path (created if missing).
// When the url query parameter is given, the body is expected to be
// empty and the tar stream is downloaded from that http(s) URL instead.
// The repeatable include and exclude query parameters limit which
// entries are extracted, with the same pattern syntax as GET /v1/tar;
// entries not passing the filter are skipped. Regular files and
// directories are supported; absolute entry names and entries escaping
// the destination are rejected. Existing files are overwritten; on a
// mid-archive error the files extracted so far remain.
func TarPutHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		rawURL := q.Get("url")
		if rawURL != "" {
			if err := validatePutURL(rawURL); err != nil {
				writeError(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		filter, err := newTarFilter(q)
		if err != nil {
			writeError(w, err.Error(), http.StatusBadRequest)
			return
		}
		p, unlock, ok := requirePath(w, r.URL.Query())
		if !ok {
			return
		}
		defer unlock()
		src, ok := putSource(w, r, rawURL)
		if !ok {
			return
		}
		defer src.Close()
		if err := os.MkdirAll(p, 0o755); err != nil {
			writeError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := extractTar(src, p, filter); err != nil {
			if errors.Is(err, errBadTar) {
				writeError(w, err.Error(), http.StatusBadRequest)
			} else {
				writeError(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		writeMessage(w, http.StatusOK, "OK")
	}
}

func extractTar(rd io.Reader, dest string, filter *tarFilter) error {
	tr := tar.NewReader(rd)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar: %w: %v", errBadTar, err)
		}
		target, err := joinTarEntry(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if !filter.included(hdr.Name) {
				continue
			}
			if err := os.MkdirAll(target, tarPerm(hdr, 0o755)); err != nil {
				return fmt.Errorf("tar: %s: %w", hdr.Name, err)
			}
			// MkdirAll leaves existing directories untouched and applies
			// umask to new ones, so restore the archive mode explicitly.
			if err := os.Chmod(target, tarPerm(hdr, 0o755)); err != nil {
				return fmt.Errorf("tar: %s: %w", hdr.Name, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if !filter.included(hdr.Name) {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("tar: %s: %w", hdr.Name, err)
			}
			if err := writeTarEntry(tr, target, hdr); err != nil {
				return fmt.Errorf("tar: %s: %w", hdr.Name, err)
			}
		default:
			return fmt.Errorf("tar: %w: unsupported entry type %d (%s)", errBadTar, hdr.Typeflag, hdr.Name)
		}
	}
}

// joinTarEntry maps a tar entry name to a path inside dest, rejecting
// absolute names and ".." traversal escaping dest.
func joinTarEntry(dest, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("tar: %w: absolute entry name %q", errBadTar, name)
	}
	target := filepath.Join(dest, name)
	if target != dest && !strings.HasPrefix(target, dest+string(os.PathSeparator)) {
		return "", fmt.Errorf("tar: %w: entry %q escapes destination", errBadTar, name)
	}
	return target, nil
}

func writeTarEntry(tr *tar.Reader, target string, hdr *tar.Header) error {
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, tarPerm(hdr, 0o644))
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, tr); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// The OpenFile mode is subject to umask and ignored when the file
	// already exists, so restore the archive mode explicitly.
	if err := os.Chmod(target, tarPerm(hdr, 0o644)); err != nil {
		return err
	}
	if !hdr.ModTime.IsZero() {
		// Best effort: content matters more than timestamps.
		_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
	}
	return nil
}

// tarPerm returns the entry's permission bits, falling back to def when
// the archive carries none.
func tarPerm(hdr *tar.Header, def os.FileMode) os.FileMode {
	if m := hdr.FileInfo().Mode().Perm(); m != 0 {
		return m
	}
	return def
}
