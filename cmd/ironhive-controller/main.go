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

	"github.com/yankeguo/ironhive/controller"
)

func main() {
	var config string
	flag.StringVar(&config, "config", controller.DefaultConfigPath, "config file path")
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

	// The Kubernetes client is best-effort at startup: without it the API
	// still serves, and the absence is loud in the logs.
	kube, source, err := controller.NewKubernetesClient(cfg.Kubernetes.Kubeconfig)
	if err != nil {
		log.Println("kubernetes client unavailable:", err)
	} else {
		log.Println("kubernetes client ready:", source)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The pod manager keeps standby pods warm; like the client it is
	// best-effort — without a cluster the API still serves. It runs
	// even with zero pools configured: reconcile then sweeps whatever
	// managed pods a previous configuration left behind.
	var pm *controller.PodManager
	if kube != nil {
		pm = controller.NewPodManager(kube, cfg.Kubernetes.Namespace, cfg)
		go pm.Run(ctx)               // watch loop, every replica
		go pm.RunLeaderElection(ctx) // reconcile loop, leader only
		log.Printf("pod manager started in namespace %s with %d pool(s)", cfg.Kubernetes.Namespace, len(cfg.Pools))
	} else {
		log.Println("pod manager disabled: no kubernetes client")
	}

	// No server timeouts of any kind: proxied shell SSE streams and
	// tar/file transfers legitimately run for the request's whole
	// lifetime, and slow or stalled peers are the upstream's concern —
	// it hits its own limits long before we would, so this process must
	// not be the bottleneck. Concurrency is likewise left unbounded.
	srv := &http.Server{
		Addr:    cfg.HTTP.Listen,
		Handler: controller.NewServer(kube, cfg, pm).Handler(),
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
