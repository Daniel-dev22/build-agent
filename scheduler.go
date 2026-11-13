package main

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// Agent-wide admission control.
//
// Lane limits and the memory budget used to be channels created INSIDE each run(), so every job got
// its own fresh semaphores: a "slow lane of 1" bounded one job's services and nothing across jobs,
// and two concurrent builds each ran their own slow service. The scheduler below is built ONCE per
// agent (newEngine) and shared by every job.

// weightedSem is a FIFO weighted semaphore. FIFO matters: a large reservation (an Android build)
// must not be starved forever by a stream of small ones that always fit.
type weightedSem struct {
	mu      sync.Mutex
	size    int64
	used    int64
	waiters list.List // of *semWaiter
}

type semWaiter struct {
	n     int64
	ready chan struct{}
}

func newWeightedSem(size int64) *weightedSem { return &weightedSem{size: size} }

// acquire blocks until n units are free or ctx ends. n > size can never be granted and is refused
// up front rather than queued forever.
func (s *weightedSem) acquire(ctx context.Context, n int64) error {
	if n <= 0 {
		return nil
	}
	s.mu.Lock()
	if n > s.size {
		s.mu.Unlock()
		return fmt.Errorf("request of %d exceeds the limit %d", n, s.size)
	}
	if s.waiters.Len() == 0 && s.used+n <= s.size {
		s.used += n
		s.mu.Unlock()
		return nil
	}
	w := &semWaiter{n: n, ready: make(chan struct{})}
	el := s.waiters.PushBack(w)
	s.mu.Unlock()

	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		select {
		case <-w.ready:
			// Granted in the same instant the context ended: hand it back, the caller sees an error.
			s.mu.Unlock()
			s.release(n)
		default:
			s.waiters.Remove(el)
			// Removing the head may unblock those behind it.
			s.grantLocked()
			s.mu.Unlock()
		}
		return ctx.Err()
	}
}

func (s *weightedSem) release(n int64) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	s.used -= n
	s.grantLocked()
	s.mu.Unlock()
}

// grantLocked wakes waiters from the head while they fit. Caller holds s.mu.
func (s *weightedSem) grantLocked() {
	for {
		front := s.waiters.Front()
		if front == nil {
			return
		}
		w := front.Value.(*semWaiter)
		if s.used+w.n > s.size {
			return
		}
		s.used += w.n
		s.waiters.Remove(front)
		close(w.ready)
	}
}

// scheduler owns the agent-wide limits: one semaphore per lane and, when a memory ceiling is
// configured, one weighted semaphore whose capacity is that ceiling.
type scheduler struct {
	fast, slow *weightedSem
	mem        *weightedSem // nil when no ceiling is configured
	memDefault int64
	running    atomic.Int32 // services currently admitted (inside buildx)
}

func newScheduler(cfg Config) *scheduler {
	s := &scheduler{
		fast: newWeightedSem(int64(max(cfg.FastLane, 1))),
		slow: newWeightedSem(int64(max(cfg.SlowLane, 1))),
	}
	if cfg.MemoryCeiling > 0 {
		s.mem = newWeightedSem(cfg.MemoryCeiling)
		s.memDefault = cfg.MemoryDefault
	}
	return s
}

// runLimits is the per-job refinement of the agent-wide lane limits. A descriptor's lanes.fast/slow
// may only LOWER the agent limit (a repo-supplied manifest must never raise concurrency past what the
// operator allows), so a job gets an extra, smaller semaphore for a lane only when it asks for fewer.
type runLimits struct {
	fast, slow *weightedSem // nil = no per-job restriction on that lane
}

func (s *scheduler) limitsFor(d Descriptor) runLimits {
	var rl runLimits
	if d.Lanes.Fast > 0 && int64(d.Lanes.Fast) < s.fast.size {
		rl.fast = newWeightedSem(int64(d.Lanes.Fast))
	}
	if d.Lanes.Slow > 0 && int64(d.Lanes.Slow) < s.slow.size {
		rl.slow = newWeightedSem(int64(d.Lanes.Slow))
	}
	return rl
}

// weight is the memory reservation a service is charged: its own `memory`, else the agent default.
func (s *scheduler) weight(svc ServiceSpec) int64 {
	if s.mem == nil {
		return 0
	}
	if n, err := parseBytes(svc.Memory); err == nil && n > 0 {
		return n
	}
	return s.memDefault
}

// admit blocks until the service may start: a per-job lane slot (if the descriptor lowered it), then
// the agent-wide lane slot, then its memory reservation. The returned release must be called once.
// Order is fixed (lane before memory) so two waiters can never hold what the other needs.
func (s *scheduler) admit(ctx context.Context, rl runLimits, svc ServiceSpec) (func(), error) {
	lane, local := s.fast, rl.fast
	if svc.lane() == "slow" {
		lane, local = s.slow, rl.slow
	}
	var undo []func()
	release := func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
	take := func(sem *weightedSem, n int64) error {
		if sem == nil || n <= 0 {
			return nil
		}
		if err := sem.acquire(ctx, n); err != nil {
			return err
		}
		undo = append(undo, func() { sem.release(n) })
		return nil
	}
	if err := take(local, 1); err != nil {
		release()
		return nil, err
	}
	if err := take(lane, 1); err != nil {
		release()
		return nil, err
	}
	if err := take(s.mem, s.weight(svc)); err != nil {
		release()
		return nil, fmt.Errorf("memory reservation: %w", err)
	}
	s.running.Add(1)
	inner := release
	var once sync.Once
	return func() { once.Do(func() { s.running.Add(-1); inner() }) }, nil
}
