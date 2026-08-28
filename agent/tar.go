package agent

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// TarGetHandler serves GET /v1/tar?path=<dir>: the directory at path is
// streamed back as an uncompressed tar archive attachment named
// "<dirname>.tar". Entry names are relative to the directory itself, so
// the archive round-trips through PUT /v1/tar into any destination.
// Directories and regular files are included with their modes and mtimes;
// symlinks and other special files are skipped. path may be absolute, or
// relative to the process working directory.
func TarGetHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, unlock, ok := requirePath(w, r)
		if !ok {
			return
		}
		defer unlock()
		st, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "not found: "+p, http.StatusNotFound)
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		if !st.IsDir() {
			http.Error(w, "not a directory: "+p, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment",
			map[string]string{"filename": st.Name() + ".tar"}))
		// The status is already sent, so a mid-stream failure can only
		// truncate the response.
		tw := tar.NewWriter(w)
		_ = writeDirToTar(tw, p)
		_ = tw.Close()
	}
}

// writeDirToTar walks root and writes its contents as tar entries named
// relative to root.
func writeDirToTar(tw *tar.Writer, root string) error {
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

// errBadTar marks errors caused by the archive itself (corrupt stream,
// malicious entry names, unsupported entry types) as opposed to local
// filesystem failures, so the handler can answer 400 instead of 500.
var errBadTar = errors.New("invalid tar archive")

// TarPutHandler serves PUT /v1/tar?path=<dir>: the request body is an
// uncompressed tar stream extracted into path (created if missing).
// When the url query parameter is given, the body is expected to be
// empty and the tar stream is downloaded from that http(s) URL instead.
// Regular files and directories are supported; absolute entry names and
// entries escaping the destination are rejected. Existing files are
// overwritten; on a mid-archive error the files extracted so far remain.
func TarPutHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rawURL := r.URL.Query().Get("url")
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
		src, ok := putSource(w, r, rawURL)
		if !ok {
			return
		}
		defer src.Close()
		if err := os.MkdirAll(p, 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := extractTar(src, p); err != nil {
			if errors.Is(err, errBadTar) {
				http.Error(w, err.Error(), http.StatusBadRequest)
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
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
			return fmt.Errorf("tar: %w: %v", errBadTar, err)
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
