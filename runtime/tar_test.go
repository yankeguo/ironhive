package runtime

import (
	"archive/tar"
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
