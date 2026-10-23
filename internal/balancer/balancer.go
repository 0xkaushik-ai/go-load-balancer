// Package balancer implements an HTTP load balancer (a reverse proxy that
// spreads requests across several backend servers).
//
// For each incoming request the Balancer:
//
//  1. picks a healthy backend using a Strategy (round-robin or least-conn)
//  2. forwards the request with net/http/httputil.ReverseProxy
//  3. if the backend cannot be reached, marks it dead and, when it is safe,
//     retries the request on another backend
//
// A background health checker (see health.go) brings dead backends back once
// they answer again.
package balancer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// StatusPath is reserved on the balancer for its JSON status page.
// Requests to it are answered by the balancer, not forwarded.
const StatusPath = "/lb/status"

// Config configures a Balancer.
type Config struct {
	Backends       []string      // backend base URLs, e.g. "http://127.0.0.1:9101"
	Strategy       string        // "round-robin" or "least-conn"
	HealthPath     string        // path polled on each backend, e.g. "/health"
	HealthInterval time.Duration // time between health checks
}

// Balancer is an http.Handler that load-balances requests across backends.
type Balancer struct {
	cfg      Config
	log      *slog.Logger
	backends []*Backend
	strategy Strategy
}

// New validates cfg and builds a Balancer. All backends start out alive;
// call RunHealthChecks to keep that status up to date.
func New(cfg Config, log *slog.Logger) (*Balancer, error) {
	if len(cfg.Backends) == 0 {
		return nil, errors.New("at least one backend is required")
	}
	strategy, err := NewStrategy(cfg.Strategy)
	if err != nil {
		return nil, err
	}
	lb := &Balancer{cfg: cfg, log: log, strategy: strategy}

	// One Transport shared by every backend keeps a pool of reusable
	// connections. A short dial timeout makes a dead backend fail fast.
	transport := &http.Transport{
		Proxy:                 nil, // never send backend traffic through HTTP_PROXY
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}

	for _, raw := range cfg.Backends {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("invalid backend URL %q (want http://host:port)", raw)
		}
		b := &Backend{URL: u}
		b.alive.Store(true)
		b.proxy = &httputil.ReverseProxy{
			// Rewrite points the outgoing request at this backend and sets the
			// X-Forwarded-For/-Host/-Proto headers so the backend knows the real client.
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(u)
				pr.SetXForwarded()
			},
			Transport: transport,
			// Tag responses so you can see which backend answered.
			ModifyResponse: func(resp *http.Response) error {
				resp.Header.Set("X-Backend", u.Host)
				return nil
			},
			ErrorHandler: lb.proxyError(b),
		}
		lb.backends = append(lb.backends, b)
	}
	return lb, nil
}

// Backends returns the configured backends (for status and tests).
func (lb *Balancer) Backends() []*Backend { return lb.backends }

// attempt tracks one incoming request across retries. It travels inside the
// request context so the proxy's ErrorHandler can reach it.
type attempt struct {
	req    *http.Request
	tried  map[*Backend]bool
	failed bool // at least one backend failed
	retry  bool // set by ErrorHandler: try another backend
}

type attemptKey struct{}

// ServeHTTP is the entry point for every client request.
func (lb *Balancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == StatusPath {
		lb.serveStatus(w)
		return
	}

	at := &attempt{tried: make(map[*Backend]bool)}
	at.req = r.WithContext(context.WithValue(r.Context(), attemptKey{}, at))

	for {
		b := lb.pick(at.tried)
		if b == nil {
			if at.failed {
				http.Error(w, "bad gateway: all backends failed", http.StatusBadGateway)
			} else {
				http.Error(w, "service unavailable: no healthy backends", http.StatusServiceUnavailable)
			}
			return
		}
		at.tried[b] = true
		at.retry = false

		b.requests.Add(1)
		b.active.Add(1)
		b.proxy.ServeHTTP(w, at.req)
		b.active.Add(-1)

		if !at.retry {
			return
		}
		lb.log.Info("retrying on another backend", "method", r.Method, "path", r.URL.Path)
	}
}

// pick returns a live backend that has not been tried yet, or nil.
func (lb *Balancer) pick(tried map[*Backend]bool) *Backend {
	candidates := make([]*Backend, 0, len(lb.backends))
	for _, b := range lb.backends {
		if b.Alive() && !tried[b] {
			candidates = append(candidates, b)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	return lb.strategy.Next(candidates)
}

// proxyError returns the ErrorHandler for backend b. ReverseProxy calls it
// when the backend could not be reached or did not answer in time. In that
// case nothing has been written to the client yet, so a retry is possible.
func (lb *Balancer) proxyError(b *Backend) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		// The client gave up. That is not the backend's fault.
		if errors.Is(err, context.Canceled) {
			return
		}

		b.failures.Add(1)
		if b.setAlive(false) {
			lb.log.Warn("backend marked down", "backend", b.URL.Host, "error", err)
		}

		at, _ := r.Context().Value(attemptKey{}).(*attempt)
		if at != nil {
			at.failed = true
			if canRetry(r) {
				at.retry = true // ServeHTTP will try the next backend
				return
			}
		}
		http.Error(w, "bad gateway: backend failed", http.StatusBadGateway)
	}
}

// canRetry reports whether sending r again is safe. Only read-only methods
// without a body qualify: a POST might already have been processed by the
// failed backend (and its body has been consumed), so repeating it could
// duplicate an order, payment, etc.
func canRetry(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return r.Body == nil || r.Body == http.NoBody
	}
	return false
}

// backendStatus is one row of the status page.
type backendStatus struct {
	URL      string `json:"url"`
	Alive    bool   `json:"alive"`
	Active   int64  `json:"active"`
	Requests uint64 `json:"requests"`
	Failures uint64 `json:"failures"`
}

// serveStatus writes the balancer's view of every backend as JSON.
func (lb *Balancer) serveStatus(w http.ResponseWriter) {
	out := struct {
		Strategy string          `json:"strategy"`
		Backends []backendStatus `json:"backends"`
	}{Strategy: lb.cfg.Strategy}

	for _, b := range lb.backends {
		out.Backends = append(out.Backends, backendStatus{
			URL:      b.URL.String(),
			Alive:    b.Alive(),
			Active:   b.Active(),
			Requests: b.requests.Load(),
			Failures: b.failures.Load(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}
