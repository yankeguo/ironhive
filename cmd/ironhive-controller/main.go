package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yankeguo/ironhive/controller"
)

func main() {
	listen := envOr("IRONHIVE_LISTEN", ":8080")
	flag.StringVar(&listen, "listen", listen, "http listen address")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              listen,
		Handler:           controller.NewServer().Handler(),
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
