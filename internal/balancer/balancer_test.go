package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newBackend starts a test backend that answers with its name.
func newBackend(t *testing.T, name string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, name)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newBalancer(t *testing.T, strategy string, urls ...string) *Balancer {
	t.Helper()
	lb, err := New(Config{
		Backends:       urls,
		Strategy:       strategy,
		HealthPath:     "/health",
		HealthInterval: 50 * time.Millisecond,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return lb
}

// get sends one request through the balancer and returns status and body.
func get(t *testing.T, lb http.Handler, method, path string, body io.Reader) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	lb.ServeHTTP(rec, httptest.NewRequest(method, path, body))
	return rec.Code, rec.Body.String()
}

func TestRoundRobin(t *testing.T) {
	a, b, c := newBackend(t, "A"), newBackend(t, "B"), newBackend(t, "C")
	lb := newBalancer(t, "round-robin", a.URL, b.URL, c.URL)

	var got []string
	for range 6 {
		_, body := get(t, lb, "GET", "/", nil)
		got = append(got, body)
	}
	if want := "A B C A B C"; strings.Join(got, " ") != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
}

func TestLeastConn(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		fmt.Fprint(w, "slow")
	}))
	defer slow.Close()
	defer close(release)
	fast := newBackend(t, "fast")

	lb := newBalancer(t, "least-conn", slow.URL, fast.URL)

	// First request: both idle, so it goes to the first backend and blocks there.
	go get(t, lb, "GET", "/", nil)
	waitFor(t, func() bool { return lb.Backends()[0].Active() == 1 })

	// Every request now goes to the idle backend.
	for range 3 {
		if _, body := get(t, lb, "GET", "/", nil); body != "fast" {
			t.Fatalf("got %q, want fast (the backend with fewer connections)", body)
		}
	}
}

func TestFailover(t *testing.T) {
	a := newBackend(t, "A")
	dead := newBackend(t, "dead")
	dead.Close() // connections to it are now refused
	lb := newBalancer(t, "round-robin", dead.URL, a.URL)

	// The first pick is the dead backend. The request must still succeed via A.
	code, body := get(t, lb, "GET", "/", nil)
	if code != 200 || body != "A" {
		t.Fatalf("got %d %q, want 200 A", code, body)
	}
	if lb.Backends()[0].Alive() {
		t.Fatal("dead backend should be marked down")
	}
}

func TestNoRetryForPOST(t *testing.T) {
	a := newBackend(t, "A")
	dead := newBackend(t, "dead")
	dead.Close()
	lb := newBalancer(t, "round-robin", dead.URL, a.URL)

	// A POST that fails is not repeated, because the failed backend might
	// already have processed it.
	if code, _ := get(t, lb, "POST", "/", strings.NewReader("data")); code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502", code)
	}
}

func TestAllBackendsDown(t *testing.T) {
	dead := newBackend(t, "dead")
	dead.Close()
	lb := newBalancer(t, "round-robin", dead.URL)

	if code, _ := get(t, lb, "GET", "/", nil); code != http.StatusBadGateway {
		t.Fatalf("first request: got %d, want 502 (the backend failed)", code)
	}
	if code, _ := get(t, lb, "GET", "/", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("second request: got %d, want 503 (no healthy backends)", code)
	}
}

func TestHealthCheckRecovery(t *testing.T) {
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" && !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	lb := newBalancer(t, "round-robin", srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go lb.RunHealthChecks(ctx)

	b := lb.Backends()[0]
	waitFor(t, func() bool { return !b.Alive() })
	healthy.Store(true)
	waitFor(t, func() bool { return b.Alive() })
}

func TestStatusPage(t *testing.T) {
	a := newBackend(t, "A")
	lb := newBalancer(t, "round-robin", a.URL)
	get(t, lb, "GET", "/", nil)

	code, body := get(t, lb, "GET", StatusPath, nil)
	if code != 200 {
		t.Fatalf("status code %d", code)
	}
	var st struct {
		Strategy string
		Backends []backendStatus
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st.Strategy != "round-robin" || len(st.Backends) != 1 || st.Backends[0].Requests != 1 || !st.Backends[0].Alive {
		t.Fatalf("unexpected status: %+v", st)
	}
}

func TestBadConfig(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, cfg := range []Config{
		{Strategy: "round-robin"},
		{Backends: []string{"http://x:1"}, Strategy: "fastest"},
		{Backends: []string{"localhost:9000"}, Strategy: "round-robin"},
	} {
		if _, err := New(cfg, log); err == nil {
			t.Errorf("New(%+v) succeeded, want error", cfg)
		}
	}
}

// waitFor polls cond until it is true or two seconds pass.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
