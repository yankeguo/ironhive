package runtime

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/files/get?path="+url.QueryEscape(path), nil)
	rec := httptest.NewRecorder()
	FilesGetHandler().ServeHTTP(rec, req)
	return rec
}

func TestFilesGetAbsolute(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hello.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := get(t, p)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != "hello" {
		t.Fatalf("body = %q, want %q", body, "hello")
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="hello.txt"` {
		t.Fatalf("Content-Disposition = %q", cd)
	}
}

func TestFilesGetRelative(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rel.txt"), []byte("relative"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	rec := get(t, "rel.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != "relative" {
		t.Fatalf("body = %q, want %q", body, "relative")
	}
}

func TestFilesGetErrors(t *testing.T) {
	if rec := get(t, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing path: status = %d, want 400", rec.Code)
	}
	if rec := get(t, filepath.Join(t.TempDir(), "nope")); rec.Code != http.StatusNotFound {
		t.Fatalf("nonexistent: status = %d, want 404", rec.Code)
	}
	if rec := get(t, t.TempDir()); rec.Code != http.StatusBadRequest {
		t.Fatalf("directory: status = %d, want 400", rec.Code)
	}
}
