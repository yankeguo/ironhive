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
	listen := envOr("IHC_LISTEN", ":8080")
	kubeconfig := envOr("IHC_KUBECONFIG", "")
	config := envOr("IHC_CONFIG", "config.yml")
	namespace := envOr("IHC_NAMESPACE", controller.DefaultNamespace())
	flag.StringVar(&listen, "listen", listen, "http listen address")
	flag.StringVar(&kubeconfig, "kubeconfig", kubeconfig,
		"kubeconfig path; defaults to the standard loading rules, with in-cluster config as fallback")
	flag.StringVar(&config, "config", config, "config file path")
	flag.StringVar(&namespace, "namespace", namespace, "namespace managed pods live in")
	flag.Parse()

	// The config file is optional while nothing consumes it yet: absent is
	// fine, present but invalid is a hard failure — misconfiguration must
	// not boot silently.
	cfg, err := controller.LoadConfig(config)
	switch {
	case err == nil:
		log.Printf("config loaded from %s: %d pool(s)", config, len(cfg.Pools))
	case errors.Is(err, fs.ErrNotExist):
		log.Println("no config file at", config, "— running without pools")
	default:
		log.Println("config:", err)
		os.Exit(1)
	}

	// The Kubernetes client is best-effort at startup: without it the web
	// UI still serves, and the absence is loud in the logs.
	kube, source, err := controller.NewKubernetesClient(kubeconfig)
	if err != nil {
		log.Println("kubernetes client unavailable:", err)
	} else {
		log.Println("kubernetes client ready:", source)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The pod manager keeps standby pods warm; like the client and config
	// it is best-effort — without either one the web UI still serves.
	var pm *controller.PodManager
	if kube != nil && cfg != nil {
		pm = controller.NewPodManager(kube, namespace, cfg)
		go pm.Run(ctx)
		log.Printf("pod manager started in namespace %s", namespace)
	} else {
		log.Println("pod manager disabled: no config file or no kubernetes client")
	}

	srv := &http.Server{
		Addr:              listen,
		Handler:           controller.NewServer(kube, cfg, pm).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Println("listening on", listen)
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
