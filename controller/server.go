package controller

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// allocateWait is how long POST /controller/v1/allocate blocks waiting for
// a standby pod to become Ready before giving up with 503.
const allocateWait = 30 * time.Second

type Server struct {
	// Kubernetes is the client for the cluster hosting the managed
	// containers; nil when no credentials were resolvable at startup.
	Kubernetes kubernetes.Interface
	// Config is the loaded config file, or the default config when no
	// config file was found; never nil.
	Config *Config
	// Pods tracks the managed pods and hands them out; nil when the pod
	// manager is disabled (no config or no cluster).
	Pods *PodManager

	// agentURL builds the base URL of a sandbox pod's agent; a field so
	// tests can point it at a stub server.
	agentURL func(PodState) string
}

func NewServer(kube kubernetes.Interface, cfg *Config, pm *PodManager) *Server {
	s := &Server{Kubernetes: kube, Config: cfg, Pods: pm}
	s.agentURL = s.defaultAgentURL
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /controller/v1/allocate", s.handleAllocate)
	mux.HandleFunc("POST /controller/v1/release", s.handleRelease)
	mux.HandleFunc("POST /controller/v1/renew", s.handleRenew)
	mux.HandleFunc("GET /controller/v1/pools", s.handlePools)
	mux.HandleFunc("/agent/", s.handleAgentProxy)
	return s.withSecurityHeaders(mux)
}

// withSecurityHeaders applies generic API hygiene headers. Framing is not
// restricted: there is no HTML to frame, and access control is a
// deployment-level concern layered in front of the controller.
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"message": "OK"})
}

// handleAllocate hands one Ready standby pod of the requested pool to the
// caller, blocking up to allocateWait for one to become available. The
// pool name and the mandatory lease duration are read from the query
// string or a urlencoded body, like the agent's POST endpoints.
func (s *Server) handleAllocate(w http.ResponseWriter, r *http.Request) {
	if s.Pods == nil {
		writeError(w, http.StatusServiceUnavailable, "pod manager disabled")
		return
	}
	params, ok := formParams(w, r)
	if !ok {
		return
	}
	pool := params.Get("pool")
	if pool == "" {
		writeError(w, http.StatusBadRequest, "missing pool")
		return
	}
	lease, ok := leaseParam(w, params)
	if !ok {
		return
	}
	st, err := s.Pods.Allocate(r.Context(), pool, lease, allocateWait)
	switch {
	case errors.Is(err, ErrUnknownPool):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNoSandboxAvailable):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case err != nil:
		log.Println("allocate:", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	default:
		writeJSON(w, http.StatusOK, map[string]string{
			"sandbox":      st.Name,
			"leaseExpires": st.LeaseExpires.UTC().Format(time.RFC3339),
		})
	}
}

// handleRenew extends the lease of an allocated pod to lease from now.
func (s *Server) handleRenew(w http.ResponseWriter, r *http.Request) {
	if s.Pods == nil {
		writeError(w, http.StatusServiceUnavailable, "pod manager disabled")
		return
	}
	params, ok := formParams(w, r)
	if !ok {
		return
	}
	name := params.Get("sandbox")
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing sandbox")
		return
	}
	lease, ok := leaseParam(w, params)
	if !ok {
		return
	}
	st, err := s.Pods.Renew(r.Context(), name, lease)
	switch {
	case errors.Is(err, ErrSandboxNotFound), errors.Is(err, ErrSandboxNotAllocated):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		log.Println("renew:", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	default:
		writeJSON(w, http.StatusOK, map[string]string{
			"sandbox":      st.Name,
			"leaseExpires": st.LeaseExpires.UTC().Format(time.RFC3339),
		})
	}
}

// leaseParam reads the mandatory lease duration parameter (a Go duration
// string like "5m"); on failure it writes the error and returns ok ==
// false.
func leaseParam(w http.ResponseWriter, params url.Values) (time.Duration, bool) {
	raw := params.Get("lease")
	if raw == "" {
		writeError(w, http.StatusBadRequest, "missing lease")
		return 0, false
	}
	lease, err := time.ParseDuration(raw)
	if err != nil || lease <= 0 {
		writeError(w, http.StatusBadRequest, "invalid lease: "+raw)
		return 0, false
	}
	return lease, true
}

// handleRelease destroys an allocated pod; the pool is topped up with a
// fresh standby pod asynchronously. The sandbox name is read from the
// query string or a urlencoded body, like the agent's POST endpoints.
func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	if s.Pods == nil {
		writeError(w, http.StatusServiceUnavailable, "pod manager disabled")
		return
	}
	params, ok := formParams(w, r)
	if !ok {
		return
	}
	name := params.Get("sandbox")
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing sandbox")
		return
	}
	err := s.Pods.Release(r.Context(), name)
	switch {
	case errors.Is(err, ErrSandboxNotFound), errors.Is(err, ErrSandboxNotAllocated):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		log.Println("release:", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	default:
		writeJSON(w, http.StatusOK, map[string]string{"released": name})
	}
}

// poolSummary aggregates the pod counts of one pool for the overview API.
type poolSummary struct {
	Name      string `json:"name"`
	Standby   int    `json:"standby"`
	Pending   int    `json:"pending"`
	Allocated int    `json:"allocated"`
}

// podInfo is the API view of one managed pod.
type podInfo struct {
	Name         string `json:"name"`
	Pool         string `json:"pool"`
	Phase        string `json:"phase"`
	Ready        bool   `json:"ready"`
	IP           string `json:"ip,omitempty"`
	Deleting     bool   `json:"deleting,omitempty"`
	Allocated    bool   `json:"allocated"`
	LeaseExpires string `json:"leaseExpires,omitempty"`
	CreatedAt    string `json:"createdAt"`
}

// handlePools serves the read-only cluster overview: per-pool
// standby/pending/allocated counts plus every managed pod. It is
// unauthenticated and CORS-open by design — the data is not sensitive and
// third-party systems are encouraged to embed it.
func (s *Server) handlePools(w http.ResponseWriter, _ *http.Request) {
	if s.Pods == nil {
		writeError(w, http.StatusServiceUnavailable, "pod manager disabled")
		return
	}
	summaries := make(map[string]*poolSummary, len(s.Config.Pools))
	for name := range s.Config.Pools {
		summaries[name] = &poolSummary{Name: name}
	}
	pods := s.Pods.Pods()
	infos := make([]podInfo, 0, len(pods))
	for _, p := range pods {
		sum, ok := summaries[p.Pool]
		if !ok {
			sum = &poolSummary{Name: p.Pool}
			summaries[p.Pool] = sum
		}
		switch {
		case p.Deleting:
			// Terminating pods are visible in the table but are no longer
			// usable standby or allocated capacity.
		case p.Allocated:
			sum.Allocated++
		case p.Phase == corev1.PodRunning && p.Ready:
			sum.Standby++
		case p.Phase != corev1.PodSucceeded && p.Phase != corev1.PodFailed:
			sum.Pending++
		}
		info := podInfo{
			Name:      p.Name,
			Pool:      p.Pool,
			Phase:     string(p.Phase),
			Ready:     p.Ready,
			IP:        p.IP,
			Deleting:  p.Deleting,
			Allocated: p.Allocated,
			CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339),
		}
		if p.Allocated && !p.LeaseExpires.IsZero() {
			info.LeaseExpires = p.LeaseExpires.UTC().Format(time.RFC3339)
		}
		infos = append(infos, info)
	}
	ordered := make([]poolSummary, 0, len(summaries))
	for _, sum := range summaries {
		ordered = append(ordered, *sum)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeJSON(w, http.StatusOK, map[string]any{"pools": ordered, "pods": infos})
}

// handleAgentProxy forwards any request under /agent/ to the agent inside
// the sandbox pod named by the X-Sandbox-ID header, preserving the path —
// the agent serves its own API under /agent/v1/....
func (s *Server) handleAgentProxy(w http.ResponseWriter, r *http.Request) {
	if s.Pods == nil {
		writeError(w, http.StatusServiceUnavailable, "pod manager disabled")
		return
	}
	name := r.Header.Get("X-Sandbox-ID")
	st, ok := s.Pods.Lookup(name)
	if !ok || st.Deleting || !st.Allocated || st.IP == "" {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	target, err := url.Parse(s.agentURL(st))
	if err != nil {
		log.Println("agent proxy target:", name, ":", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Println("agent proxy:", name, ":", err)
			writeError(w, http.StatusBadGateway, "upstream agent unavailable")
		},
	}
	proxy.ServeHTTP(w, r)
}

// defaultAgentURL targets the agent's listen address inside the pod.
func (s *Server) defaultAgentURL(st PodState) string {
	var pool PoolConfig
	if p, ok := s.Config.Pools[st.Pool]; ok {
		pool = p
	}
	return "http://" + net.JoinHostPort(st.IP, strconv.Itoa(int(pool.AgentPort())))
}

// formParams parses and returns the merged query and urlencoded form-body
// parameters of a POST request — both are allowed, and body entries take
// precedence over query entries on conflicts. On failure it writes the
// error response and returns ok == false.
func formParams(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid form: "+err.Error())
		return nil, false
	}
	return r.Form, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}
