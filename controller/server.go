package controller

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
)

// allocateWait is how long POST /controller/v1/allocate blocks waiting for
// a standby pod to become Ready before giving up with 503.
const allocateWait = 30 * time.Second

type Server struct {
	// Kubernetes is the client for the cluster hosting the managed
	// containers; nil when no credentials were resolvable at startup.
	Kubernetes kubernetes.Interface
	// Config is the loaded config.yml; nil when no config file was found.
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
	mux.HandleFunc("/agent/", s.handleAgentProxy)
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /about", s.handleAbout)
	mux.Handle("GET /static/", staticHandler())
	return s.withSecurityHeaders(mux)
}

func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", strings.Join([]string{
			"default-src 'none'",
			"script-src 'self'",
			"style-src 'self' 'unsafe-inline'",
			"img-src 'self' data:",
			"connect-src 'self'",
			"form-action 'self'",
			"base-uri 'none'",
			"frame-ancestors 'none'",
		}, "; "))
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"message": "OK"})
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	s.render(w, "home.html", map[string]any{
		"Nav":  "home",
		"Time": time.Now().Format(time.DateTime),
	})
}

func (s *Server) handleAbout(w http.ResponseWriter, r *http.Request) {
	s.render(w, "about.html", map[string]any{
		"Nav": "about",
	})
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := webTmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Println("template:", err)
	}
}

// handleAllocate hands one Ready standby pod of the requested pool to the
// caller, blocking up to allocateWait for one to become available. The pool
// name is read from the query string or a urlencoded body, like the
// agent's POST endpoints.
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
	st, err := s.Pods.Allocate(r.Context(), pool, allocateWait)
	switch {
	case errors.Is(err, ErrUnknownPool):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNoSandboxAvailable):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]string{"sandbox": st.Name})
	}
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
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]string{"released": name})
	}
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
	if !ok || !st.Allocated || st.IP == "" {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	target, err := url.Parse(s.agentURL(st))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Println("agent proxy:", name, ":", err)
			writeError(w, http.StatusBadGateway, err.Error())
		},
	}
	proxy.ServeHTTP(w, r)
}

// defaultAgentURL targets the agent's listen address inside the pod.
func (s *Server) defaultAgentURL(st PodState) string {
	port := DefaultAgentPort
	if s.Config != nil {
		if pool, ok := s.Config.Pools[st.Pool]; ok {
			port = pool.Agent.Port
		}
	}
	return "http://" + net.JoinHostPort(st.IP, strconv.Itoa(port))
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
