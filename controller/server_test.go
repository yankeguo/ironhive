package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// testServerWithPods builds a server backed by a pod manager with one
// Ready standby pod.
func testServerWithPods(t *testing.T) *Server {
	t.Helper()
	cfg := testPoolConfig(1)
	m := NewPodManager(fake.NewSimpleClientset(), "ironhive", cfg)
	m.reconcile(context.Background())
	markReady(m)
	return NewServer(nil, cfg, m)
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func allocateViaHTTP(t *testing.T, base string) string {
	t.Helper()
	resp := postJSON(t, base+"/controller/v1/allocate", map[string]string{"pool": "default"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("allocate: status %d", resp.StatusCode)
	}
	var out struct {
		Sandbox string `json:"sandbox"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Sandbox == "" {
		t.Fatal("allocate: empty sandbox name")
	}
	return out.Sandbox
}

func TestAllocateReleaseEndpoints(t *testing.T) {
	srv := httptest.NewServer(testServerWithPods(t).Handler())
	defer srv.Close()

	name := allocateViaHTTP(t, srv.URL)

	resp := postJSON(t, srv.URL+"/controller/v1/release", map[string]string{"sandbox": name})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release: status %d", resp.StatusCode)
	}

	// Releasing the same sandbox again is a 404.
	resp = postJSON(t, srv.URL+"/controller/v1/release", map[string]string{"sandbox": name})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("re-release: status %d, want 404", resp.StatusCode)
	}

	// An unknown pool is a 400.
	resp = postJSON(t, srv.URL+"/controller/v1/allocate", map[string]string{"pool": "nope"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("allocate unknown pool: status %d, want 400", resp.StatusCode)
	}
}

func TestEndpointsWithoutPodManager(t *testing.T) {
	srv := httptest.NewServer(NewServer(nil, testPoolConfig(1), nil).Handler())
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/controller/v1/allocate", map[string]string{"pool": "default"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("allocate: status %d, want 503", resp.StatusCode)
	}

	resp = postJSON(t, srv.URL+"/controller/v1/release", map[string]string{"sandbox": "sandbox-x"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("release: status %d, want 503", resp.StatusCode)
	}

	resp, err := http.Get(srv.URL + "/agent/v1/file")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("agent proxy: status %d, want 503", resp.StatusCode)
	}
}

func TestAgentProxy(t *testing.T) {
	s := testServerWithPods(t)

	// A stub agent records what it receives.
	var gotMethod, gotPath, gotQuery string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		_, _ = w.Write([]byte("agent says hi"))
	}))
	defer agent.Close()
	s.agentURL = func(PodState) string { return agent.URL }

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	name := allocateViaHTTP(t, srv.URL)

	req, err := http.NewRequest(http.MethodPut, srv.URL+"/agent/v1/file?path=/tmp/x", strings.NewReader("data"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Sandbox-ID", name)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy: status %d", resp.StatusCode)
	}
	if gotMethod != http.MethodPut || gotPath != "/agent/v1/file" || gotQuery != "path=/tmp/x" {
		t.Errorf("agent received %s %s?%s", gotMethod, gotPath, gotQuery)
	}

	// Without the header there is nothing to proxy to.
	resp, err = http.Get(srv.URL + "/agent/v1/file")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("proxy without sandbox id: status %d, want 404", resp.StatusCode)
	}
}
