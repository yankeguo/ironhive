package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

func dirTarget(path, extraQuery string) string {
	return "/v1/dir?path=" + url.QueryEscape(path) + extraQuery
}

func TestDirGetList(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("12345"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "a-sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, dirTarget(dir, ""), nil)
	rec := httptest.NewRecorder()
	DirGetHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	var entries []dirEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %v", entries)
	}
	// ReadDir sorts by name.
	if entries[0].Name != "a-sub" || !entries[0].Dir || entries[0].Mode != "0750" {
		t.Fatalf("entries[0] = %+v", entries[0])
	}
	if entries[1].Name != "b.txt" || entries[1].Dir || entries[1].Size != 5 || entries[1].Mode != "0640" {
		t.Fatalf("entries[1] = %+v", entries[1])
	}
	if entries[1].Mtime == "" {
		t.Fatalf("missing mtime: %+v", entries[1])
	}
}

func TestDirGetErrors(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/dir", nil)
	rec := httptest.NewRecorder()
	DirGetHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing path: status = %d, want 400", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, dirTarget(filepath.Join(t.TempDir(), "nope"), ""), nil)
	rec = httptest.NewRecorder()
	DirGetHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nonexistent: status = %d, want 404", rec.Code)
	}

	f := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, dirTarget(f, ""), nil)
	rec = httptest.NewRecorder()
	DirGetHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("not a directory: status = %d, want 500", rec.Code)
	}
}

func TestDirPutCreate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a", "b", "c")
	req := httptest.NewRequest(http.MethodPut, dirTarget(p, ""), nil)
	rec := httptest.NewRecorder()
	DirPutHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !st.IsDir() {
		t.Fatalf("not a directory: %s", p)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o, want 755", st.Mode().Perm())
	}
}

func TestDirPutChmodChown(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	p := filepath.Join(t.TempDir(), "owned")
	req := httptest.NewRequest(http.MethodPut,
		dirTarget(p, "&chmod=0700&chown="+url.QueryEscape(cur.Uid+":"+cur.Gid)), nil)
	rec := httptest.NewRecorder()
	DirPutHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %o, want 700", st.Mode().Perm())
	}
}

func TestDirPutErrors(t *testing.T) {
	dir := t.TempDir()
	req := httptest.NewRequest(http.MethodPut, "/v1/dir", nil)
	rec := httptest.NewRecorder()
	DirPutHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing path: status = %d, want 400", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPut, dirTarget(filepath.Join(dir, "a"), "&chmod=755"), nil)
	rec = httptest.NewRecorder()
	DirPutHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("chmod without leading 0: status = %d, want 400", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPut, dirTarget(filepath.Join(dir, "a"), "&chown=no-such-user-xyz"), nil)
	rec = httptest.NewRecorder()
	DirPutHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown chown user: status = %d, want 400", rec.Code)
	}
}
