package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/file?path="+url.QueryEscape(path), nil)
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
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename=hello.txt` {
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

func put(t *testing.T, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	FilesPutHandler().ServeHTTP(rec, req)
	return rec
}

func putTarget(path, extraQuery string) string {
	return "/v1/file?path=" + url.QueryEscape(path) + extraQuery
}

func TestFilesPutBasic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "up.txt")
	rec := put(t, putTarget(p, ""), "uploaded")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "uploaded" {
		t.Fatalf("content = %q, want %q", data, "uploaded")
	}
}

func TestFilesPutRelative(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	rec := put(t, putTarget("rel-up.txt", ""), "relative")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "rel-up.txt")); string(data) != "relative" {
		t.Fatalf("content = %q", data)
	}
}

func TestFilesPutChmod(t *testing.T) {
	p := filepath.Join(t.TempDir(), "script.sh")
	rec := put(t, putTarget(p, "&chmod=0755"), "#!/bin/sh\n")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o, want 755", st.Mode().Perm())
	}
}

func TestFilesPutChown(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	// Numeric uid:gid, uid only, gid only, and name forms — all chown-ing
	// to ourselves, which any file owner may do.
	cases := []string{
		cur.Uid + ":" + cur.Gid,
		cur.Uid,
		":" + cur.Gid,
		cur.Username,
	}
	if grp, err := user.LookupGroupId(cur.Gid); err == nil {
		cases = append(cases, cur.Username+":"+grp.Name)
	}
	for _, chown := range cases {
		p := filepath.Join(t.TempDir(), "owned.txt")
		rec := put(t, putTarget(p, "&chown="+url.QueryEscape(chown)), "x")
		if rec.Code != http.StatusOK {
			t.Fatalf("chown %q: status = %d, want 200 (%s)", chown, rec.Code, rec.Body)
		}
	}
}

func TestFilesPutErrors(t *testing.T) {
	dir := t.TempDir()
	if rec := put(t, "/v1/file", "x"); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing path: status = %d, want 400", rec.Code)
	}
	if rec := put(t, putTarget(filepath.Join(dir, "a"), "&chmod=644"), "x"); rec.Code != http.StatusBadRequest {
		t.Fatalf("chmod without leading 0: status = %d, want 400", rec.Code)
	}
	if rec := put(t, putTarget(filepath.Join(dir, "a"), "&chmod=0899"), "x"); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid octal chmod: status = %d, want 400", rec.Code)
	}
	if rec := put(t, putTarget(filepath.Join(dir, "a"), "&chown=no-such-user-xyz:grp"), "x"); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown chown user: status = %d, want 400", rec.Code)
	}
	if rec := put(t, putTarget(filepath.Join(dir, "no-such-dir", "deep", "a"), ""), "x"); rec.Code != http.StatusOK {
		t.Fatalf("missing parent dirs are auto-created: status = %d (%s)", rec.Code, rec.Body)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "no-such-dir", "deep", "a")); string(data) != "x" {
		t.Fatalf("content = %q", data)
	}
}

func TestFilesPutAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "data.txt")
	if rec := put(t, putTarget(p, ""), "v1"); rec.Code != http.StatusOK {
		t.Fatalf("first put: status = %d (%s)", rec.Code, rec.Body)
	}
	// Overwrite: the rename must replace the existing file.
	if rec := put(t, putTarget(p, ""), "v2-longer-content"); rec.Code != http.StatusOK {
		t.Fatalf("second put: status = %d (%s)", rec.Code, rec.Body)
	}
	if data, _ := os.ReadFile(p); string(data) != "v2-longer-content" {
		t.Fatalf("content = %q", data)
	}
	// The temp file must not be left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "data.txt" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("unexpected files in dir: %v", names)
	}
}

func TestFilesConcurrentSamePath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "shared.txt")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf("value-%02d", i)
			for j := 0; j < 20; j++ {
				if rec := put(t, putTarget(p, ""), body); rec.Code != http.StatusOK {
					t.Errorf("put: status = %d (%s)", rec.Code, rec.Body)
					return
				}
				rec := get(t, p)
				if rec.Code != http.StatusOK {
					t.Errorf("get: status = %d", rec.Code)
					return
				}
				b, _ := io.ReadAll(rec.Body)
				if s := string(b); !strings.HasPrefix(s, "value-") || len(s) != len(body) {
					t.Errorf("partial or corrupt read: %q", s)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}
