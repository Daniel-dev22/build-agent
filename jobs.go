package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// buildRegistry owns in-flight Builds, drives the engine, and fans lifecycle
// changes out to the event hook (durable outbox) + fleet hub. It is the single
// entry point, so its in-process lane scheduling (inside engine.run) replaces
// the Python orchestrator's cross-process FIFO.
type buildRegistry struct {
	cfg    Config
	eng    *engine
	events *eventBuffer

	hook   JobEventHook
	fleet  interface{ Trigger() }
	emitMu sync.Mutex // serialize hook emits (concurrent service goroutines)

	mu     sync.Mutex
	builds map[string]*Build
}

func newBuildRegistry(cfg Config, eng *engine, events *eventBuffer) *buildRegistry {
	return &buildRegistry{cfg: cfg, eng: eng, events: events, builds: map[string]*Build{}}
}

func (r *buildRegistry) setHook(h JobEventHook)            { r.hook = h }
func (r *buildRegistry) setFleet(f interface{ Trigger() }) { r.fleet = f }

// fleetTrigger is a lightweight fleet-only nudge (no controller event, no
// persistence) used by the engine to broadcast live buildx sub-step changes.
func (r *buildRegistry) fleetTrigger() {
	if r.fleet != nil {
		r.fleet.Trigger()
	}
}

func (r *buildRegistry) onChange(b *Build) func(JobEvent) {
	return func(evt JobEvent) {
		r.emitMu.Lock()
		if r.hook != nil {
			r.hook(b, evt)
		}
		r.emitMu.Unlock()
		if r.fleet != nil {
			r.fleet.Trigger()
		}
	}
}

// start registers a new build and runs it in the background. Returns immediately
// with the job id so the trigger caller (ansible) can poll.
func (r *buildRegistry) start(parentCtx context.Context, req BuildRequest) *Build {
	id := uuid.NewString()
	dir := filepath.Join(r.cfg.WorkDir, id)
	_ = os.MkdirAll(dir, 0o700)

	jobCtx, cancel := context.WithCancel(parentCtx)
	b := &Build{
		buildPublic: buildPublic{
			ID: id, Project: req.Descriptor.Project, Ref: req.Ref,
			Registry: req.Registry, State: BuildPending, TriggerKey: req.TriggerKey,
		},
		cancel:      cancel,
		dir:         dir,
		subscribers: map[chan string]struct{}{},
	}

	r.mu.Lock()
	r.builds[id] = b
	r.evictOldLocked()
	r.mu.Unlock()

	go func() {
		defer cancel()
		ctx, c2 := context.WithTimeout(jobCtx, r.cfg.BuildTimeout)
		defer c2()
		r.eng.run(ctx, b, req, r.onChange(b), r.fleetTrigger)
	}()
	return b
}

func (r *buildRegistry) get(id string) (*Build, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.builds[id]
	return b, ok
}

// cancelJob cancels a running build; returns false if unknown/already terminal.
func (r *buildRegistry) cancelJob(id string) bool {
	b, ok := r.get(id)
	if !ok {
		return false
	}
	b.mu.Lock()
	terminal := b.State == BuildCompleted || b.State == BuildFailed || b.State == BuildCancelled
	if !terminal {
		b.State = BuildCancelled
		b.CompletedAt = time.Now().UTC()
	}
	cancel := b.cancel
	b.mu.Unlock()
	if terminal {
		return false
	}
	if cancel != nil {
		cancel()
	}
	r.onChange(b)(EventCancelled)
	return true
}

// list merges active in-memory builds with recent persisted builds (in-memory
// wins on id collision), newest first.
func (r *buildRegistry) list() []buildPublic {
	r.mu.Lock()
	active := make([]buildPublic, 0, len(r.builds))
	seen := map[string]struct{}{}
	for id, b := range r.builds {
		active = append(active, b.snapshot())
		seen[id] = struct{}{}
	}
	r.mu.Unlock()

	recent, _ := r.events.listRecentBuilds(recentJobsRetained)
	out := active
	for _, bp := range recent {
		if _, dup := seen[bp.ID]; dup {
			continue
		}
		out = append(out, bp)
	}
	return out
}

// evictOldLocked drops terminal in-memory builds beyond the retention window so
// the map can't grow unbounded. Caller holds r.mu.
func (r *buildRegistry) evictOldLocked() {
	if len(r.builds) <= recentJobsRetained {
		return
	}
	type kv struct {
		id string
		at int64
	}
	var terminal []kv
	for id, b := range r.builds {
		st := b.snapshot()
		if st.State == BuildCompleted || st.State == BuildFailed || st.State == BuildCancelled {
			terminal = append(terminal, kv{id, st.CompletedAt.UnixNano()})
		}
	}
	for i := 0; i < len(terminal); i++ {
		for j := i + 1; j < len(terminal); j++ {
			if terminal[j].at < terminal[i].at {
				terminal[i], terminal[j] = terminal[j], terminal[i]
			}
		}
	}
	excess := len(r.builds) - recentJobsRetained
	for i := 0; i < excess && i < len(terminal); i++ {
		delete(r.builds, terminal[i].id)
	}
}

// --- log streaming (job log WS) ---

func (r *buildRegistry) subscribe(id string) (<-chan string, []string, func(), bool) {
	b, ok := r.get(id)
	if !ok {
		return nil, nil, nil, false
	}
	ch := make(chan string, subscriberBuffer)
	b.mu.Lock()
	backlog := append([]string(nil), b.ringBuffer...)
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()
	unsub := func() {
		b.mu.Lock()
		delete(b.subscribers, ch)
		b.mu.Unlock()
		close(ch)
	}
	return ch, backlog, unsub, true
}
