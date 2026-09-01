package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yankeguo/ironhive/agent"
)

func main() {
	agent.ReapZombies()

	var config string
	flag.StringVar(&config, "config", agent.DefaultConfigPath, "config file path")
	flag.Parse()

	// The config file carries every setting except its own path: absent
	// is fine (defaults), present but invalid is a hard failure —
	// misconfiguration must not boot silently.
	cfg, err := agent.LoadConfig(config)
	switch {
	case err == nil:
		log.Println("config loaded from", config)
	case errors.Is(err, fs.ErrNotExist):
		log.Println("no config file at", config, "— running with defaults")
		cfg = agent.NewConfig()
	default:
		log.Println("config:", err)
		os.Exit(1)
	}

	// bash is a hard dependency (wrapper syntax and caller commands), so
	// an image without a working one fails the boot — loudly, in the pod
	// logs — instead of failing the first shell call.
	if err := agent.CheckBash(); err != nil {
		log.Println("fatal:", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"message":"OK"}`))
	})
	// Everything except /healthz lives under /agent, leaving the root
	// namespace free for other tooling sharing the port.
	mux.HandleFunc("GET /agent/v1/file", agent.FilesGetHandler())
	mux.HandleFunc("PUT /agent/v1/file", agent.FilesPutHandler())
	mux.HandleFunc("POST /agent/v1/file/upload", agent.FilesUploadHandler())
	mux.HandleFunc("GET /agent/v1/tar", agent.TarGetHandler())
	mux.HandleFunc("PUT /agent/v1/tar", agent.TarPutHandler())
	mux.HandleFunc("POST /agent/v1/tar/upload", agent.TarUploadHandler())
	mux.HandleFunc("GET /agent/v1/dir", agent.DirGetHandler())
	mux.HandleFunc("PUT /agent/v1/dir", agent.DirPutHandler())
	mux.HandleFunc("POST /agent/v1/shell", agent.ShellPostHandler(cfg))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              cfg.HTTP.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Println("listening on", cfg.HTTP.Listen)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Println("exited with error:", err)
			os.Exit(1)
		}
		return
	case <-ctx.Done():
		log.Println("shutting down")
	}

	// Unregister the signal handler so a second SIGINT/SIGTERM terminates
	// immediately, then wait for in-flight requests with no deadline.
	stop()
	if err := srv.Shutdown(context.Background()); err != nil {
		log.Println("shutdown:", err)
		os.Exit(1)
	}
}
