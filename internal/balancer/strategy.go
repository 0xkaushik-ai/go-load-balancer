package balancer

import (
	"fmt"
	"sync/atomic"
)

// Strategy picks which backend serves the next request.
//
// candidates holds only backends that are alive and not yet tried for this
// request, and is never empty. Implementations must be safe for concurrent use.
type Strategy interface {
	Next(candidates []*Backend) *Backend
}

// NewStrategy returns the strategy with the given name.
func NewStrategy(name string) (Strategy, error) {
	switch name {
	case "round-robin":
		return &roundRobin{}, nil
	case "least-conn":
		return leastConn{}, nil
	default:
		return nil, fmt.Errorf("unknown strategy %q (want round-robin or least-conn)", name)
	}
}

// roundRobin hands out backends in turn: A, B, C, A, B, C, ...
// It works best when all backends are similar and requests cost about the same.
type roundRobin struct {
	counter atomic.Uint64
}

func (rr *roundRobin) Next(candidates []*Backend) *Backend {
	n := rr.counter.Add(1) - 1
	return candidates[n%uint64(len(candidates))]
}

// leastConn picks the backend with the fewest requests in flight. It adapts
// when some requests are slow, because busy backends naturally get less work.
// Ties go to the earliest backend in the list.
type leastConn struct{}

func (leastConn) Next(candidates []*Backend) *Backend {
	best := candidates[0]
	for _, b := range candidates[1:] {
		if b.Active() < best.Active() {
			best = b
		}
	}
	return best
}
