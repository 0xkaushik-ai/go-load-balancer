// Command lb runs the HTTP load balancer.
//
// Usage:
//
//	go run ./cmd/lb -backends http://127.0.0.1:9101,http://127.0.0.1:9102 \
//	    [-addr :8000] [-strategy round-robin|least-conn] \
//	    [-health-path /health] [-health-interval 5s]
//
// The balancer's own status is at http://<addr>/lb/status.
// Stop it with Ctrl+C. See README.md for details.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"loadbalancer/internal/balancer"
)

func main() {
	addr := flag.String("addr", ":8000", "address the balancer listens on")
	backends := flag.String("backends", "", "comma-separated backend URLs (required)")
	strategy := flag.String("strategy", "round-robin", "round-robin or least-conn")
	healthPath := flag.String("health-path", "/health", "path polled on each backend")
	healthInterval := flag.Duration("health-interval", 5*time.Second, "time between health checks")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	var urls []string
	for _, u := range strings.Split(*backends, ",") {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}

	lb, err := balancer.New(balancer.Config{
		Backends:       urls,
		Strategy:       *strategy,
		HealthPath:     *healthPath,
		HealthInterval: *healthInterval,
	}, log)
	if err != nil {
		log.Error("invalid configuration", "error", err)
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go lb.RunHealthChecks(ctx)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           lb,
		ReadHeaderTimeout: 5 * time.Second,
		// WriteTimeout must cover the slowest backend response, since the
		// balancer writes to the client while the backend is still answering.
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Info("load balancer starting", "addr", *addr, "strategy", *strategy, "backends", urls)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	log.Info("load balancer stopped")
}
