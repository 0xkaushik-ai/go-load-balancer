package balancer

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// RunHealthChecks polls every backend's health endpoint right away and then
// every cfg.HealthInterval, until ctx is cancelled. Run it in its own goroutine.
//
// This is the "active" half of health checking. The "passive" half is in
// proxyError, which marks a backend down as soon as a real request fails.
// Only active checks bring a backend back up.
func (lb *Balancer) RunHealthChecks(ctx context.Context) {
	client := &http.Client{Timeout: 2 * time.Second}

	lb.checkAll(ctx, client)
	ticker := time.NewTicker(lb.cfg.HealthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lb.checkAll(ctx, client)
		}
	}
}

// checkAll checks all backends in parallel, so one slow backend does not
// delay the others.
func (lb *Balancer) checkAll(ctx context.Context, client *http.Client) {
	var wg sync.WaitGroup
	for _, b := range lb.backends {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lb.check(ctx, client, b)
		}()
	}
	wg.Wait()
}

// check marks b alive if GET <backend><HealthPath> returns a 2xx status.
func (lb *Balancer) check(ctx context.Context, client *http.Client, b *Backend) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL.JoinPath(lb.cfg.HealthPath).String(), nil)
	if err != nil {
		return
	}

	healthy := false
	resp, err := client.Do(req)
	if err == nil {
		healthy = resp.StatusCode >= 200 && resp.StatusCode < 300
		// Drain the body so the connection can be reused for the next check.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
	}
	if ctx.Err() != nil {
		return // shutting down; the result means nothing
	}

	if b.setAlive(healthy) {
		if healthy {
			lb.log.Info("backend is up", "backend", b.URL.Host)
		} else {
			lb.log.Warn("backend failed health check", "backend", b.URL.Host, "error", err)
		}
	}
}
