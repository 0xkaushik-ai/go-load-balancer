# Go HTTP Load Balancer

An HTTP load balancer (reverse proxy) written in Go using **only the standard library**.
It spreads incoming requests across several backend servers, checks their health, and fails over automatically when one goes down.

```
                         ┌──► backend one   :9101
client ──► balancer ─────┼──► backend two   :9102   (dead → skipped)
           :8000         └──► backend three :9103
```

The project is meant for learning. It shows how to:

- build a reverse proxy with `net/http/httputil.ReverseProxy`
- implement load-balancing strategies: **round-robin** and **least-connections**
- check health both **actively** (polling `/health`) and **passively** (noticing failed requests)
- retry failed requests on another backend, and know when it's **not safe** to retry
- share counters between goroutines safely with `sync/atomic`
- test proxies with `httptest` servers

---

## What a load balancer does

A single server can only handle so much traffic, and when it crashes your site goes down. A load balancer sits in front of **several copies** of your server:

- **Scale.** Requests are spread out, so three servers handle about three times the traffic.
- **Availability.** If one server dies, the balancer stops sending it traffic, and users never notice.
- **Zero-downtime deploys.** You can take servers out one at a time to update them.

Real-world examples are Nginx, HAProxy, Envoy and cloud load balancers such as AWS ALB. This project is a small version of the same idea.

---

## Requirements

- Go **1.22 or newer**

## Quick start: the demo

One command starts three demo backends and the balancer:

```bash
cd load_balancer
make demo                      # round-robin
make demo STRATEGY=least-conn  # least-connections
```

In another terminal:

```bash
for i in $(seq 6); do curl -s 127.0.0.1:8000/; done
# backend one handled GET /
# backend two handled GET /
# backend three handled GET /
# backend one handled GET /
# ...

curl -s 127.0.0.1:8000/lb/status     # JSON health and traffic report
curl -si 127.0.0.1:8000/ | grep X-Backend   # which backend answered
```

**Try failover.** Kill one backend, for example with `pkill -f "name two"`, and keep sending requests. Every request still succeeds. Start it again and the balancer picks it back up within one health-check interval.

### Running the pieces yourself

```bash
go run ./cmd/backend -addr :9101 -name one
go run ./cmd/backend -addr :9102 -name two -delay 300ms
go run ./cmd/lb -addr :8000 -backends http://127.0.0.1:9101,http://127.0.0.1:9102
```

### Balancer options (`cmd/lb`)

| Flag               | Default       | Meaning |
|--------------------|---------------|---------|
| `-backends`        | (required)    | Backend URLs, separated by commas |
| `-addr`            | `:8000`       | Address the balancer listens on |
| `-strategy`        | `round-robin` | `round-robin` or `least-conn` |
| `-health-path`     | `/health`     | Path requested on each backend for health checks |
| `-health-interval` | `5s`          | Time between health checks |

### Demo backend options (`cmd/backend`)

| Flag     | Default    | Meaning |
|----------|------------|---------|
| `-addr`  | `:9101`    | Address to listen on |
| `-name`  | the address | Name shown in responses |
| `-delay` | `0s`       | How long to wait before each response. Use it to simulate a slow server. |

### Make targets

| Command      | What it does |
|--------------|--------------|
| `make demo`  | Run 3 backends and the balancer. Ctrl+C stops everything. |
| `make build` | Build `bin/lb` and `bin/backend` |
| `make test`  | Run tests with the race detector |
| `make vet`   | Run `go vet` static analysis |
| `make fmt`   | Format the code |

---

## Strategies

| Strategy      | How it picks a backend | Best when |
|---------------|------------------------|-----------|
| `round-robin` | In turn: A, B, C, A, B, C, … using an atomic counter | Backends are equal and requests cost about the same |
| `least-conn`  | The backend with the **fewest requests in flight** | Request times vary. Slow backends naturally get less work. |

To see the difference, run `make demo STRATEGY=least-conn`. Backend "three" has a 500 ms delay, so under concurrent load it receives fewer requests than the others.

Adding a new strategy, such as random or weighted, means implementing one method:

```go
type Strategy interface {
    Next(candidates []*Backend) *Backend
}
```

---

## Health checking

| Kind        | How it works | What it's for |
|-------------|--------------|---------------|
| **Passive** | When a real request to a backend fails (connection refused or a timeout), the backend is marked **down** immediately. | Reacting fast, on the very first failure |
| **Active**  | Every `-health-interval`, the balancer sends `GET <backend>/health` to every backend in parallel. A 2xx response marks it **up**; anything else marks it **down**. | Noticing problems while there's no traffic, and **bringing backends back** |

A backend that is down is skipped until an active check sees it healthy again.

## Failover and retries

When a backend fails, `ReverseProxy` calls the balancer's `ErrorHandler` **before anything has been sent to the client**. That makes it possible to try again on a different backend:

```
GET /page ─► backend two  ✗ connection refused   → mark two down
          ─► backend three ✓ 200 OK               → client gets 200
```

Each backend is tried **at most once** per request.

**When it does not retry.** Only `GET`, `HEAD` and `OPTIONS` requests with no body are retried. A `POST /pay` might have reached the backend before it crashed, so sending it again could charge someone twice. Those requests get `502 Bad Gateway` instead.

### Response codes from the balancer

| Code  | When |
|-------|------|
| `502 Bad Gateway`         | A backend failed and the request couldn't be retried, or every backend failed |
| `503 Service Unavailable` | No backend is currently healthy |

Every successful response includes an **`X-Backend`** header naming the backend that served it.

## Status page

`GET /lb/status` is answered by the balancer itself and is never forwarded:

```json
{
  "strategy": "round-robin",
  "backends": [
    { "url": "http://127.0.0.1:9101", "alive": true,  "active": 0, "requests": 5, "failures": 0 },
    { "url": "http://127.0.0.1:9102", "alive": false, "active": 0, "requests": 3, "failures": 1 }
  ]
}
```

| Field      | Meaning |
|------------|---------|
| `alive`    | Whether the backend is currently healthy |
| `active`   | Requests to it in flight right now |
| `requests` | Total requests sent to it since startup |
| `failures` | Total proxy errors for it since startup |

---

## Project structure

```
load_balancer/
├── cmd/
│   ├── lb/main.go              # balancer entry point: flags, server, graceful shutdown
│   └── backend/main.go         # demo backend server
├── internal/
│   └── balancer/
│       ├── balancer.go         # ServeHTTP, picking, retries, error handling, status page
│       ├── backend.go          # Backend type and its atomic counters
│       ├── strategy.go         # round-robin and least-connections
│       ├── health.go           # active health checker
│       └── balancer_test.go    # tests with real httptest backends
├── scripts/demo.sh             # starts 3 backends + the balancer
├── Makefile
├── go.mod
└── README.md
```

## Request flow

```
ServeHTTP(request)
   │
   ├─ path is /lb/status? ──► write JSON, done
   │
   └─ loop:
        pick(): backends that are alive and not yet tried ──► Strategy.Next()
           │  none left? ──► 502 (something failed) or 503 (nothing healthy)
           ▼
        active++ ; ReverseProxy.ServeHTTP ; active--
           │
           ├─ success ──► response streamed to client (+ X-Backend header)
           │
           └─ ErrorHandler: mark backend down
                 ├─ safe to retry? ──► loop again with the next backend
                 └─ not safe        ──► 502
```

## Design notes

- **One shared `http.Transport`.** It keeps a pool of reusable connections to the backends, which is much faster than opening a new TCP connection for every request. A 2-second dial timeout makes a dead backend fail quickly, and `Proxy: nil` stops `HTTP_PROXY` environment variables from redirecting backend traffic.
- **`Rewrite` instead of the older `Director`.** `pr.SetURL` points the request at the backend. `pr.SetXForwarded` adds `X-Forwarded-For`, `X-Forwarded-Host` and `X-Forwarded-Proto`, so backends can still see the real client IP. Unlike `Director`, `Rewrite` also strips any forwarded headers the client sent, so a client can't fake its IP address.
- **Atomics, not mutexes.** Each backend's `alive`, `active`, `requests` and `failures` are atomic values. The hot path, which picks a backend and counts the request, never takes a lock.
- **Retry state lives in the request context.** `ReverseProxy` calls `ErrorHandler` with its own copy of the request. The balancer's per-request `attempt` struct travels in the context, so the handler can flag "retry" for the main loop.
- **A client disconnect isn't a backend failure.** If the error is `context.Canceled`, the client hung up. The backend is not marked down.
- **Timeouts.** The balancer server sets `ReadHeaderTimeout` to guard against Slowloris attacks. `WriteTimeout` is 60 s and must be longer than your slowest backend's response time. The transport's `ResponseHeaderTimeout` (30 s) stops a hung backend from tying up requests forever.

## Limitations

These are left out to keep the code small:

- Request bodies aren't buffered, so `POST`/`PUT` requests are never retried.
- There are no weights, sticky sessions or circuit breaker.
- The backend list is fixed at startup; there's no hot reload.
- TLS is supported for backends (`https://` URLs), but the balancer itself serves only HTTP.

## Testing

```bash
go test -race ./...
```

The tests start real `httptest` backends and cover:

- round-robin order
- least-connections choosing the idle backend
- failover when a backend is dead
- a `POST` not being retried
- 502 versus 503 when everything is down
- the health checker bringing a backend back
- the status page
- rejection of bad configuration

## Ideas to extend

- Weighted round-robin, for when backends have different capacity
- Sticky sessions using a cookie or a hash of the client IP
- A circuit breaker, so a backend that keeps failing is skipped for longer
- TLS termination on the balancer (`ListenAndServeTLS`)
- Rate limiting per client IP
- A config file with hot reload (add and remove backends without restarting)
