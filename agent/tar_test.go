package agent

import (
	"archive/tar"
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type tarEntry struct {
	name string
	typ  byte
	mode int64
	body string
}

func buildTar(t *testing.T, entries []tarEntry) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typ,
			Mode:     e.mode,
			Size:     int64(len(e.body)),
			ModTime:  time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf
}

func putTar(t *testing.T, path string, body *bytes.Buffer) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/v1/tar?path="+url.QueryEscape(path), body)
	rec := httptest.NewRecorder()
	TarPutHandler().ServeHTTP(rec, req)
	return rec
}

func TestTarPutExtract(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	body := buildTar(t, []tarEntry{
		{name: "sub", typ: tar.TypeDir, mode: 0o755},
		{name: "top.txt", typ: tar.TypeReg, mode: 0o644, body: "top"},
		{name: "sub/nested.txt", typ: tar.TypeReg, mode: 0o600, body: "nested"},
	})
	rec := putTar(t, dest, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	for name, want := range map[string]string{
		"top.txt":        "top",
		"sub/nested.txt": "nested",
	} {
		data, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(data) != want {
			t.Fatalf("%s = %q, want %q", name, data, want)
		}
	}
	st, err := os.Stat(filepath.Join(dest, "sub", "nested.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", st.Mode().Perm())
	}
	if st.ModTime().UTC().Format(time.DateTime) != "2024-01-02 03:04:05" {
		t.Fatalf("mtime = %v", st.ModTime())
	}
}

func TestTarPutOverwrite(t *testing.T) {
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "a.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := putTar(t, dest, buildTar(t, []tarEntry{
		{name: "a.txt", typ: tar.TypeReg, mode: 0o644, body: "new"},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	if data, _ := os.ReadFile(filepath.Join(dest, "a.txt")); string(data) != "new" {
		t.Fatalf("content = %q", data)
	}
}

func TestTarPutCreatesDestination(t *testing.T) {
	// The target directory (and its parents) are created automatically.
	dest := filepath.Join(t.TempDir(), "x", "y", "z")
	rec := putTar(t, dest, buildTar(t, []tarEntry{
		{name: "f.txt", typ: tar.TypeReg, mode: 0o644, body: "deep"},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	if data, _ := os.ReadFile(filepath.Join(dest, "f.txt")); string(data) != "deep" {
		t.Fatalf("content = %q", data)
	}
}

func TestTarPutErrors(t *testing.T) {
	if rec := putTar(t, "", &bytes.Buffer{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing path: status = %d, want 400", rec.Code)
	}
	dest := t.TempDir()
	if rec := putTar(t, dest, bytes.NewBufferString("not a tar")); rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage body: status = %d, want 400", rec.Code)
	}
	if rec := putTar(t, dest, buildTar(t, []tarEntry{
		{name: "../evil.txt", typ: tar.TypeReg, mode: 0o644, body: "x"},
	})); rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal entry: status = %d, want 400", rec.Code)
	}
	if rec := putTar(t, dest, buildTar(t, []tarEntry{
		{name: "/abs.txt", typ: tar.TypeReg, mode: 0o644, body: "x"},
	})); rec.Code != http.StatusBadRequest {
		t.Fatalf("absolute entry: status = %d, want 400", rec.Code)
	}
	if rec := putTar(t, dest, buildTar(t, []tarEntry{
		{name: "link", typ: tar.TypeSymlink, mode: 0o777},
	})); rec.Code != http.StatusBadRequest {
		t.Fatalf("symlink entry: status = %d, want 400", rec.Code)
	}
}

func TestTarPutFromURL(t *testing.T) {
	body := buildTar(t, []tarEntry{
		{name: "sub", typ: tar.TypeDir, mode: 0o755},
		{name: "sub/nested.txt", typ: tar.TypeReg, mode: 0o600, body: "nested"},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body.Bytes())
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "out")
	req := httptest.NewRequest(http.MethodPut,
		"/v1/tar?path="+url.QueryEscape(dest)+"&url="+url.QueryEscape(srv.URL+"/x.tar"), nil)
	rec := httptest.NewRecorder()
	TarPutHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	if data, _ := os.ReadFile(filepath.Join(dest, "sub", "nested.txt")); string(data) != "nested" {
		t.Fatalf("content = %q", data)
	}
}

func TestTarPutFromURLErrors(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	putURL := func(target string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut,
			"/v1/tar?path="+url.QueryEscape(dest)+"&url="+url.QueryEscape(target), nil)
		rec := httptest.NewRecorder()
		TarPutHandler().ServeHTTP(rec, req)
		return rec
	}
	if rec := putURL("ftp://example.com/x.tar"); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-http url: status = %d, want 400", rec.Code)
	}
	if rec := putURL("http://127.0.0.1:1/x.tar"); rec.Code != http.StatusBadGateway {
		t.Fatalf("unreachable url: status = %d, want 502", rec.Code)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	if rec := putURL(srv.URL); rec.Code != http.StatusBadGateway {
		t.Fatalf("non-200 upstream: status = %d, want 502", rec.Code)
	}
	// A garbage tar stream from the URL is still a 400, not a 500.
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not a tar"))
	}))
	defer garbage.Close()
	if rec := putURL(garbage.URL); rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage tar from url: status = %d, want 400", rec.Code)
	}
}

func TestTarGetRoundtrip(t *testing.T) {
	// Build a source tree.
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "top.txt"), []byte("top"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "nested.txt"), []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlink must be skipped, not fail the archive.
	if err := os.Symlink("top.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	// GET the tar.
	req := httptest.NewRequest(http.MethodGet, "/v1/tar?path="+url.QueryEscape(src), nil)
	rec := httptest.NewRecorder()
	TarGetHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-tar" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".tar") {
		t.Fatalf("Content-Disposition = %q", cd)
	}

	// PUT it back into a fresh destination and compare the trees.
	dest := filepath.Join(t.TempDir(), "restore")
	putRec := putTar(t, dest, rec.Body)
	if putRec.Code != http.StatusOK {
		t.Fatalf("put: status = %d (%s)", putRec.Code, putRec.Body)
	}
	for name, want := range map[string]string{
		"top.txt":        "top",
		"sub/nested.txt": "nested",
	} {
		data, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(data) != want {
			t.Fatalf("%s = %q, want %q", name, data, want)
		}
	}
	st, err := os.Stat(filepath.Join(dest, "sub", "nested.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", st.Mode().Perm())
	}
	if _, err := os.Lstat(filepath.Join(dest, "link")); !os.IsNotExist(err) {
		t.Fatalf("symlink should have been skipped, err = %v", err)
	}
}

func TestTarGetErrors(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/tar", nil)
	rec := httptest.NewRecorder()
	TarGetHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing path: status = %d, want 400", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/tar?path="+url.QueryEscape(filepath.Join(t.TempDir(), "nope")), nil)
	rec = httptest.NewRecorder()
	TarGetHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nonexistent: status = %d, want 404", rec.Code)
	}

	f := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/tar?path="+url.QueryEscape(f), nil)
	rec = httptest.NewRecorder()
	TarGetHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("not a directory: status = %d, want 400", rec.Code)
	}
}
