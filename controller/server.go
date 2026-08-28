package controller

import (
	"log"
	"net/http"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
)

type Server struct {
	// Kubernetes is the client for the cluster hosting the managed
	// containers; nil when no credentials were resolvable at startup.
	Kubernetes kubernetes.Interface
	// Config is the loaded config.yml; nil when no config file was found.
	Config *Config
}

func NewServer(kube kubernetes.Interface, cfg *Config) *Server {
	return &Server{Kubernetes: kube, Config: cfg}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
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
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("OK"))
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
