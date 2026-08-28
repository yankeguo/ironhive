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

	"github.com/yankeguo/ironhive/controller"
)

func main() {
	config := envOr("IHC_CONFIG", "config.yml")
	flag.StringVar(&config, "config", config, "config file path")
	flag.Parse()

	// The config file carries every setting except its own path: absent
	// is fine (defaults, no pools), present but invalid is a hard
	// failure — misconfiguration must not boot silently.
	cfg, err := controller.LoadConfig(config)
	switch {
	case err == nil:
		log.Printf("config loaded from %s: %d pool(s)", config, len(cfg.Pools))
	case errors.Is(err, fs.ErrNotExist):
		log.Println("no config file at", config, "— running with defaults and no pools")
		cfg = controller.NewConfig()
	default:
		log.Println("config:", err)
		os.Exit(1)
	}

	// The Kubernetes client is best-effort at startup: without it the web
	// UI still serves, and the absence is loud in the logs.
	kube, source, err := controller.NewKubernetesClient(cfg.Kubernetes.Kubeconfig)
	if err != nil {
		log.Println("kubernetes client unavailable:", err)
	} else {
		log.Println("kubernetes client ready:", source)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The pod manager keeps standby pods warm; like the client it is
	// best-effort — without pools or a cluster the web UI still serves.
	var pm *controller.PodManager
	if kube != nil && len(cfg.Pools) > 0 {
		pm = controller.NewPodManager(kube, cfg.Kubernetes.Namespace, cfg)
		go pm.Run(ctx)               // watch loop, every replica
		go pm.RunLeaderElection(ctx) // reconcile loop, leader only
		log.Printf("pod manager started in namespace %s", cfg.Kubernetes.Namespace)
	} else {
		log.Println("pod manager disabled: no pools configured or no kubernetes client")
	}

	srv := &http.Server{
		Addr:              cfg.HTTP.Listen,
		Handler:           controller.NewServer(kube, cfg, pm).Handler(),
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
