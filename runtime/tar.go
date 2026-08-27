package runtime

import (
	"archive/tar"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// TarPutHandler serves PUT /v1/tar?path=<dir>: the request body is an
// uncompressed tar stream extracted into path (created if missing).
// Regular files and directories are supported; absolute entry names and
// entries escaping the destination are rejected. Existing files are
// overwritten; on a mid-archive error the files extracted so far remain.
func TarPutHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		if p == "" {
			http.Error(w, "missing query parameter: path", http.StatusBadRequest)
			return
		}
		p, err := resolveFilePath(p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		unlock := lockPath(p)
		defer unlock()
		if err := os.MkdirAll(p, 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := extractTar(r.Body, p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("OK"))
	}
}

func extractTar(rd io.Reader, dest string) error {
	tr := tar.NewReader(rd)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		target, err := joinTarEntry(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, tarPerm(hdr, 0o755)); err != nil {
				return fmt.Errorf("tar: %s: %w", hdr.Name, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("tar: %s: %w", hdr.Name, err)
			}
			if err := writeTarEntry(tr, target, hdr); err != nil {
				return fmt.Errorf("tar: %s: %w", hdr.Name, err)
			}
		default:
			return fmt.Errorf("tar: unsupported entry type %d (%s)", hdr.Typeflag, hdr.Name)
		}
	}
}

// joinTarEntry maps a tar entry name to a path inside dest, rejecting
// absolute names and ".." traversal escaping dest.
func joinTarEntry(dest, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("tar: absolute entry name %q", name)
	}
	target := filepath.Join(dest, name)
	if target != dest && !strings.HasPrefix(target, dest+string(os.PathSeparator)) {
		return "", fmt.Errorf("tar: entry %q escapes destination", name)
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
