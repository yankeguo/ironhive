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

func TestFilesPutFromURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("downloaded"))
	}))
	defer srv.Close()
	p := filepath.Join(t.TempDir(), "dl.txt")
	rec := put(t, putTarget(p, "&url="+url.QueryEscape(srv.URL+"/file")), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if data, _ := os.ReadFile(p); string(data) != "downloaded" {
		t.Fatalf("content = %q, want %q", data, "downloaded")
	}
	// chmod applies to downloaded content too.
	p2 := filepath.Join(t.TempDir(), "dl2.sh")
	rec = put(t, putTarget(p2, "&url="+url.QueryEscape(srv.URL)+"&chmod=0755"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if st, _ := os.Stat(p2); st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o, want 755", st.Mode().Perm())
	}
}

func TestFilesPutFromURLErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dl.txt")
	if rec := put(t, putTarget(p, "&url=ftp://example.com/x"), ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-http url: status = %d, want 400", rec.Code)
	}
	if rec := put(t, putTarget(p, "&url="+url.QueryEscape("http://127.0.0.1:1/x")), ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("unreachable url: status = %d, want 502", rec.Code)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	if rec := put(t, putTarget(p, "&url="+url.QueryEscape(srv.URL)), ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("non-200 upstream: status = %d, want 502", rec.Code)
	}
	// Failed downloads must not create the target file.
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("target should not exist after failed download: %v", err)
	}
}

func upload(t *testing.T, path, extraQuery string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v1/file/upload?path="+url.QueryEscape(path)+extraQuery, nil)
	rec := httptest.NewRecorder()
	FilesUploadHandler().ServeHTTP(rec, req)
	return rec
}

func TestFilesUpload(t *testing.T) {
	p := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(p, []byte("payload-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotMethod, gotAuth, gotBody string
	var gotLen int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotLen = r.ContentLength
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	}))
	defer srv.Close()

	rec := upload(t, p, "&url="+url.QueryEscape(srv.URL+"/up")+
		"&method=put&headers="+url.QueryEscape("Authorization=Bearer t0ken"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if gotMethod != http.MethodPut {
		t.Fatalf("method = %q, want PUT", gotMethod)
	}
	if gotAuth != "Bearer t0ken" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotBody != "payload-content" {
		t.Fatalf("body = %q", gotBody)
	}
	if gotLen != int64(len("payload-content")) {
		t.Fatalf("ContentLength = %d", gotLen)
	}

	// method defaults to POST.
	rec = upload(t, p, "&url="+url.QueryEscape(srv.URL))
	if rec.Code != http.StatusOK {
		t.Fatalf("default method: status = %d (%s)", rec.Code, rec.Body)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("default method = %q, want POST", gotMethod)
	}
}

func TestFilesUploadErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := upload(t, p, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing url: status = %d, want 400", rec.Code)
	}
	if rec := upload(t, p, "&url="+url.QueryEscape("ftp://example.com/x")); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-http url: status = %d, want 400", rec.Code)
	}
	if rec := upload(t, p, "&url="+url.QueryEscape("http://example.com")+"&method=get"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bodiless method: status = %d, want 400", rec.Code)
	}
	if rec := upload(t, p, "&url="+url.QueryEscape("http://example.com")+"&headers="+url.QueryEscape("noequals")); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad headers entry: status = %d, want 400", rec.Code)
	}
	if rec := upload(t, filepath.Join(t.TempDir(), "nope"), "&url="+url.QueryEscape("http://example.com")); rec.Code != http.StatusNotFound {
		t.Fatalf("missing file: status = %d, want 404", rec.Code)
	}
	if rec := upload(t, t.TempDir(), "&url="+url.QueryEscape("http://example.com")); rec.Code != http.StatusBadRequest {
		t.Fatalf("directory: status = %d, want 400", rec.Code)
	}
	// Upstream failure surfaces as 502 with the upstream status.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "broken", http.StatusConflict)
	}))
	defer srv.Close()
	rec := upload(t, p, "&url="+url.QueryEscape(srv.URL))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("upstream 409: status = %d, want 502", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "409") || !strings.Contains(body, "broken") {
		t.Fatalf("error body = %q, want upstream status and snippet", body)
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
