package balancer

import (
	"net/http/httputil"
	"net/url"
	"sync/atomic"
)

// Backend is one upstream server the balancer can send requests to.
//
// All counters are atomics because many request goroutines and the health
// checker read and update them at the same time.
type Backend struct {
	URL   *url.URL
	proxy *httputil.ReverseProxy

	alive    atomic.Bool   // false = skipped until a health check passes
	active   atomic.Int64  // requests in flight right now
	requests atomic.Uint64 // total requests sent here
	failures atomic.Uint64 // total proxy errors (connection refused, timeout, ...)
}

// Alive reports whether the backend is currently considered healthy.
func (b *Backend) Alive() bool { return b.alive.Load() }

// Active returns the number of requests currently in flight to this backend.
func (b *Backend) Active() int64 { return b.active.Load() }

// setAlive updates the health flag and reports whether it changed.
func (b *Backend) setAlive(alive bool) (changed bool) {
	return b.alive.Swap(alive) != alive
}
