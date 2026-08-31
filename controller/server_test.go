package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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

// postForm POSTs urlencoded form values, the way the controller's POST
// endpoints expect their parameters.
func postForm(t *testing.T, rawURL string, form url.Values) *http.Response {
	t.Helper()
	resp, err := http.PostForm(rawURL, form)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func allocateViaHTTP(t *testing.T, base string) string {
	t.Helper()
	resp := postForm(t, base+"/controller/v1/allocate", url.Values{"pool": {"default"}, "lease": {"10m"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("allocate: status %d", resp.StatusCode)
	}
	var out struct {
		Sandbox      string `json:"sandbox"`
		LeaseExpires string `json:"leaseExpires"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Sandbox == "" {
		t.Fatal("allocate: empty sandbox name")
	}
	if out.LeaseExpires == "" {
		t.Fatal("allocate: empty leaseExpires")
	}
	return out.Sandbox
}

func TestAllocateReleaseEndpoints(t *testing.T) {
	srv := httptest.NewServer(testServerWithPods(t).Handler())
	defer srv.Close()

	name := allocateViaHTTP(t, srv.URL)

	// Renewing extends the lease.
	resp := postForm(t, srv.URL+"/controller/v1/renew", url.Values{"sandbox": {name}, "lease": {"1h"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("renew: status %d", resp.StatusCode)
	}

	resp = postForm(t, srv.URL+"/controller/v1/release", url.Values{"sandbox": {name}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release: status %d", resp.StatusCode)
	}

	// Releasing the same sandbox again is a 404.
	resp = postForm(t, srv.URL+"/controller/v1/release", url.Values{"sandbox": {name}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("re-release: status %d, want 404", resp.StatusCode)
	}

	// Renewing an unknown sandbox is a 404.
	resp = postForm(t, srv.URL+"/controller/v1/renew", url.Values{"sandbox": {name}, "lease": {"1h"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("renew released sandbox: status %d, want 404", resp.StatusCode)
	}

	// Parameters may also ride in the query string.
	resp = postForm(t, srv.URL+"/controller/v1/allocate?pool=nope&lease=5m", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("allocate unknown pool via query: status %d, want 400", resp.StatusCode)
	}

	// A missing pool parameter is a 400.
	resp = postForm(t, srv.URL+"/controller/v1/allocate", url.Values{"lease": {"5m"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("allocate without pool: status %d, want 400", resp.StatusCode)
	}

	// The lease is mandatory and must be a positive duration.
	resp = postForm(t, srv.URL+"/controller/v1/allocate", url.Values{"pool": {"default"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("allocate without lease: status %d, want 400", resp.StatusCode)
	}
	resp = postForm(t, srv.URL+"/controller/v1/allocate", url.Values{"pool": {"default"}, "lease": {"banana"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("allocate with invalid lease: status %d, want 400", resp.StatusCode)
	}
	resp = postForm(t, srv.URL+"/controller/v1/allocate", url.Values{"pool": {"default"}, "lease": {"-5m"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("allocate with negative lease: status %d, want 400", resp.StatusCode)
	}
}

func TestEndpointsWithoutPodManager(t *testing.T) {
	srv := httptest.NewServer(NewServer(nil, testPoolConfig(1), nil).Handler())
	defer srv.Close()

	resp := postForm(t, srv.URL+"/controller/v1/allocate", url.Values{"pool": {"default"}, "lease": {"5m"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("allocate: status %d, want 503", resp.StatusCode)
	}

	resp = postForm(t, srv.URL+"/controller/v1/release", url.Values{"sandbox": {"sandbox-x"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("release: status %d, want 503", resp.StatusCode)
	}

	resp = postForm(t, srv.URL+"/controller/v1/renew", url.Values{"sandbox": {"sandbox-x"}, "lease": {"5m"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("renew: status %d, want 503", resp.StatusCode)
	}

	resp, err := http.Get(srv.URL + "/controller/v1/pools")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("pools: status %d, want 503", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/agent/v1/file")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("agent proxy: status %d, want 503", resp.StatusCode)
	}
}

func TestPoolsEndpoint(t *testing.T) {
	srv := httptest.NewServer(testServerWithPods(t).Handler())
	defer srv.Close()

	name := allocateViaHTTP(t, srv.URL)

	resp, err := http.Get(srv.URL + "/controller/v1/pools")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pools: status %d", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("pools: ACAO = %q, want *", resp.Header.Get("Access-Control-Allow-Origin"))
	}
	var out struct {
		Pools []struct {
			Name      string `json:"name"`
			Standby   int    `json:"standby"`
			Pending   int    `json:"pending"`
			Allocated int    `json:"allocated"`
		} `json:"pools"`
		Pods []struct {
			Name      string `json:"name"`
			Pool      string `json:"pool"`
			Allocated bool   `json:"allocated"`
		} `json:"pods"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Pools) != 1 || out.Pools[0].Name != "default" {
		t.Fatalf("pools = %+v", out.Pools)
	}
	// The single pod was allocated: 0 standby, 1 allocated.
	if out.Pools[0].Standby != 0 || out.Pools[0].Allocated != 1 {
		t.Fatalf("pool summary = %+v", out.Pools[0])
	}
	if len(out.Pods) != 1 || out.Pods[0].Name != name || !out.Pods[0].Allocated {
		t.Fatalf("pods = %+v", out.Pods)
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

func TestSecurityHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	NewServer(nil, testPoolConfig(1), nil).Handler().ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestAgentProxySanitizesUpstreamErrors(t *testing.T) {
	s := testServerWithPods(t)
	st, err := s.Pods.Allocate(context.Background(), "default", time.Minute, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rawURL := upstream.URL
	upstream.Close()
	s.agentURL = func(PodState) string { return rawURL }

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent/v1/file", nil)
	req.Header.Set("X-Sandbox-ID", st.Name)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var envelope map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["message"] != "upstream agent unavailable" ||
		strings.Contains(envelope["message"], "connect") {
		t.Fatalf("message = %q", envelope["message"])
	}
}

func TestAgentProxyRejectsDeletingSandbox(t *testing.T) {
	s := testServerWithPods(t)
	st, err := s.Pods.Allocate(context.Background(), "default", time.Minute, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s.Pods.mu.Lock()
	s.Pods.pods[st.Name].Deleting = true
	s.Pods.mu.Unlock()
	targetCalled := false
	s.agentURL = func(PodState) string {
		targetCalled = true
		return "http://agent.invalid"
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent/v1/file", nil)
	req.Header.Set("X-Sandbox-ID", st.Name)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if targetCalled {
		t.Fatal("deleting sandbox was sent to the proxy")
	}
}

func TestPoolsExcludeDeletingPodsFromCounts(t *testing.T) {
	s := testServerWithPods(t)
	pod := s.Pods.Pods()[0]
	s.Pods.mu.Lock()
	s.Pods.pods[pod.Name].Deleting = true
	s.Pods.mu.Unlock()

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/controller/v1/pools", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Pools []poolSummary `json:"pools"`
		Pods  []podInfo     `json:"pods"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Pools) != 1 || out.Pools[0].Standby != 0 || out.Pools[0].Pending != 0 ||
		out.Pools[0].Allocated != 0 {
		t.Fatalf("pool summary = %+v", out.Pools)
	}
	if len(out.Pods) != 1 || !out.Pods[0].Deleting {
		t.Fatalf("pods = %+v", out.Pods)
	}
}

func TestPostBodyOverridesQuery(t *testing.T) {
	s := testServerWithPods(t)
	body := url.Values{"pool": {"default"}, "lease": {"5m"}}.Encode()
	req := httptest.NewRequest(http.MethodPost,
		"/controller/v1/allocate?pool=missing&lease=1s", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want body parameters to win", rec.Code, rec.Body)
	}
}

func TestAgentProxySanitizesInvalidTarget(t *testing.T) {
	s := testServerWithPods(t)
	st, err := s.Pods.Allocate(context.Background(), "default", time.Minute, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s.agentURL = func(PodState) string { return "://invalid" }
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent/v1/file", nil)
	req.Header.Set("X-Sandbox-ID", st.Name)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var envelope map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["message"] != "internal error" {
		t.Fatalf("message = %q", envelope["message"])
	}
}
