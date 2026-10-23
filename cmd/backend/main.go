// Command backend is a tiny demo server to put behind the load balancer.
//
// Usage:
//
//	go run ./cmd/backend -addr :9101 -name one [-delay 0s]
//
// Endpoints:
//
//	GET /health  -> 200 "ok" (used by the balancer's health checks)
//	any other    -> "backend <name> handled <METHOD> <path>"
//
// -delay makes every response slow, which is handy for seeing the
// least-conn strategy in action.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", ":9101", "address to listen on")
	name := flag.String("name", "", "name shown in responses (default: the address)")
	delay := flag.Duration("delay", 0, "artificial delay before each response")
	flag.Parse()
	if *name == "" {
		*name = *addr
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil)).With("backend", *name)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(*delay):
		case <-r.Context().Done():
			return
		}
		log.Info("request", "method", r.Method, "path", r.URL.Path,
			"client", r.Header.Get("X-Forwarded-For"))
		fmt.Fprintf(w, "backend %s handled %s %s\n", *name, r.Method, r.URL.Path)
	})

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Info("backend starting", "addr", *addr, "delay", *delay)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server failed", "error", err)
		os.Exit(1)
	}
	log.Info("backend stopped")
}
