package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- helpers shared by the run()-driven tests --------------------------------------------------

// ctxTarB64 is base64(gzip(tar)) of a one-file build context holding a Dockerfile.
func ctxTarB64(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("FROM scratch\n")
	if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

const fakeDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeBuildx writes the --metadata-file buildx would, then calls hook (if any) while "building".
func fakeBuildx(hook func(svc ServiceSpec, args []string)) func(context.Context, *Build, ServiceSpec, []string, string, func(string)) error {
	return func(_ context.Context, _ *Build, svc ServiceSpec, args []string, _ string, _ func(string)) error {
		if hook != nil {
			hook(svc, args)
		}
		for i, a := range args {
			if a == "--metadata-file" && i+1 < len(args) {
				_ = os.WriteFile(args[i+1], []byte(`{"containerimage.digest":"`+fakeDigest+`"}`), 0o644)
			}
		}
		return nil
	}
}

// fakeDocker answers every docker call as a healthy host whose slot builders already carry the cap.
func fakeDocker(cfg Config) func(context.Context, ...string) (string, error) {
	return func(_ context.Context, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "inspect" {
			c := strconv.FormatInt(cfg.SlotMemory, 10)
			return c + " " + c, nil
		}
		return "", nil
	}
}

// newRunEngine is an engine whose every external dependency is a seam: no docker, no registry.
func newRunEngine(t *testing.T, cfg Config) *engine {
	t.Helper()
	cfg.Builder = "b"
	cfg.GitTimeout = 30 * time.Second
	if cfg.MemoryCeiling == 0 { // default: one 1g slot per lane, so lanes (not memory) bound concurrency
		cfg.SlotMemory = 1 << 30
		cfg.MemoryCeiling = int64(max(cfg.FastLane, 1)+max(cfg.SlowLane, 1)) * cfg.SlotMemory
	}
	e := newEngine(cfg, nil)
	e.docker = fakeDocker(cfg)
	return e
}

func newJob(t *testing.T) *Build {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "src")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &Build{buildPublic: buildPublic{State: BuildPending}, root: root, dir: dir, subscribers: map[chan string]struct{}{}}
}

func runJob(t *testing.T, e *engine, d Descriptor, req BuildRequest) *Build {
	t.Helper()
	b := newJob(t)
	req.Descriptor = d
	req.Registry = "127.0.0.1:1" // refuses instantly: best-effort registry calls no-op
	if req.Repo == "" {
		req.ContextB64 = ctxTarB64(t)
	}
	e.run(context.Background(), b, req, func(JobEvent) {}, func() {})
	return b
}

// overlapProbe records the peak number of builds running at once.
type overlapProbe struct{ cur, peak atomic.Int32 }

func (p *overlapProbe) hook(time.Duration) func(ServiceSpec, []string) {
	return func(ServiceSpec, []string) {
		n := p.cur.Add(1)
		for {
			pk := p.peak.Load()
			if n <= pk || p.peak.CompareAndSwap(pk, n) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond)
		p.cur.Add(-1)
	}
}

// ---- weightedSem -------------------------------------------------------------------------------

func TestWeightedSemBlocksAtCapacityAndReleases(t *testing.T) {
	s := newWeightedSem(2)
	ctx := context.Background()
	if err := s.acquire(ctx, 2); err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{})
	go func() { _ = s.acquire(ctx, 1); close(got) }()
	select {
	case <-got:
		t.Fatal("acquired past capacity")
	case <-time.After(40 * time.Millisecond):
	}
	s.release(1)
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("waiter not woken by release")
	}
}

func TestWeightedSemRefusesWhatCanNeverFit(t *testing.T) {
	if err := newWeightedSem(2).acquire(context.Background(), 3); err == nil {
		t.Fatal("a request larger than the whole limit must error, not queue forever")
	}
}

// A big reservation must not be starved by a stream of small ones that each fit.
func TestWeightedSemIsFIFOSoALargeWaiterIsNotStarved(t *testing.T) {
	s := newWeightedSem(10)
	ctx := context.Background()
	_ = s.acquire(ctx, 6) // 4 free
	var order []string
	var mu sync.Mutex
	note := func(n string) { mu.Lock(); order = append(order, n); mu.Unlock() }
	bigIn := make(chan struct{})
	go func() { close(bigIn); _ = s.acquire(ctx, 8); note("big") }() // needs 8: must wait for the 6 to go
	<-bigIn
	time.Sleep(30 * time.Millisecond)
	smallDone := make(chan struct{})
	go func() { _ = s.acquire(ctx, 1); note("small"); close(smallDone) }() // would fit in the 4 free, but is behind big
	select {
	case <-smallDone:
		t.Fatal("a later small request jumped the queue ahead of the large waiter")
	case <-time.After(60 * time.Millisecond):
	}
	s.release(6)
	<-smallDone
	deadline := time.After(time.Second)
	for {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n == 2 {
			return // both granted once the head fit
		}
		select {
		case <-deadline:
			t.Fatal("large waiter never granted after the capacity freed")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestWeightedSemCancelledWaiterLeavesNoLeak(t *testing.T) {
	s := newWeightedSem(1)
	_ = s.acquire(context.Background(), 1)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.acquire(ctx, 1) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("cancelled acquire must return an error")
	}
	s.release(1)
	if err := s.acquire(context.Background(), 1); err != nil {
		t.Fatalf("capacity leaked by the cancelled waiter: %v", err)
	}
}

// ---- agent-wide lanes ---------------------------------------------------------------------------

func slowSvc(name string) ServiceSpec {
	return ServiceSpec{Name: name, Dockerfile: "Dockerfile", Lane: "slow"}
}

// The live bug: the lane semaphores were created inside run(), so every job got its own and a
// "slow lane of 1" bounded one job's services and nothing across jobs. Two concurrent JOBS, one
// slow service each, must never build at the same time.
func TestSlowLaneIsAgentWideAcrossConcurrentJobs(t *testing.T) {
	e := newRunEngine(t, Config{FastLane: 2, SlowLane: 1})
	probe := &overlapProbe{}
	e.execBuildx = fakeBuildx(probe.hook(0))
	var wg sync.WaitGroup
	for _, name := range []string{"a", "b"} {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			b := runJob(t, e, Descriptor{Project: n, Services: []ServiceSpec{slowSvc(n)}}, BuildRequest{})
			if st := b.snapshot().State; st != BuildCompleted {
				t.Errorf("job %s ended %s: %s", n, st, b.snapshot().ErrorMsg)
			}
		}(name)
	}
	wg.Wait()
	if got := probe.peak.Load(); got != 1 {
		t.Fatalf("peak concurrent slow builds across jobs = %d, want 1 (lane limit must be agent-wide)", got)
	}
}

func TestFastLaneLimitAlsoBindsAcrossJobs(t *testing.T) {
	e := newRunEngine(t, Config{FastLane: 2, SlowLane: 1})
	probe := &overlapProbe{}
	e.execBuildx = fakeBuildx(probe.hook(0))
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ { // 3 jobs x 2 fast services = 6 candidates, limit 2
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := Descriptor{Project: "p", Services: []ServiceSpec{
				{Name: "x", Dockerfile: "Dockerfile"}, {Name: "y", Dockerfile: "Dockerfile"}}}
			runJob(t, e, d, BuildRequest{})
		}(i)
	}
	wg.Wait()
	if got := probe.peak.Load(); got != 2 {
		t.Fatalf("peak concurrent fast builds = %d, want exactly the agent limit 2", got)
	}
}

// A repo-supplied descriptor may lower a lane for its own job, never raise it past the operator's.
func TestDescriptorLanesCanOnlyLowerTheAgentLimit(t *testing.T) {
	e := newRunEngine(t, Config{FastLane: 1, SlowLane: 1})
	probe := &overlapProbe{}
	e.execBuildx = fakeBuildx(probe.hook(0))
	d := Descriptor{Project: "p", Lanes: LaneSpec{Fast: 8, Slow: 8},
		Services: []ServiceSpec{{Name: "x", Dockerfile: "Dockerfile"}, {Name: "y", Dockerfile: "Dockerfile"}}}
	runJob(t, e, d, BuildRequest{})
	if got := probe.peak.Load(); got != 1 {
		t.Fatalf("descriptor lanes.fast=8 raised concurrency to %d past the agent limit 1", got)
	}

	e2 := newRunEngine(t, Config{FastLane: 4, SlowLane: 1})
	probe2 := &overlapProbe{}
	e2.execBuildx = fakeBuildx(probe2.hook(0))
	d2 := Descriptor{Project: "p", Lanes: LaneSpec{Fast: 1},
		Services: []ServiceSpec{{Name: "x", Dockerfile: "Dockerfile"}, {Name: "y", Dockerfile: "Dockerfile"}, {Name: "z", Dockerfile: "Dockerfile"}}}
	runJob(t, e2, d2, BuildRequest{})
	if got := probe2.peak.Load(); got != 1 {
		t.Fatalf("descriptor lanes.fast=1 must lower the job to 1, got %d", got)
	}
}

// ---- memory slots -------------------------------------------------------------------------------

const gib = int64(1) << 30

// 8g budget / 4g slots = 2 slots: three services overlap at most two at a time, whatever the lanes.
func TestSlotsBoundConcurrencyToCeilingOverSlot(t *testing.T) {
	e := newRunEngine(t, Config{FastLane: 4, SlowLane: 4, MemoryCeiling: 8 * gib, SlotMemory: 4 * gib})
	probe := &overlapProbe{}
	e.execBuildx = fakeBuildx(probe.hook(0))
	d := Descriptor{Project: "p", Services: []ServiceSpec{
		{Name: "a", Dockerfile: "Dockerfile"}, {Name: "b", Dockerfile: "Dockerfile"}, {Name: "c", Dockerfile: "Dockerfile"}}}
	if b := runJob(t, e, d, BuildRequest{}); b.snapshot().State != BuildCompleted {
		t.Fatalf("job: %s", b.snapshot().ErrorMsg)
	}
	if got := probe.peak.Load(); got != 2 {
		t.Fatalf("8g/4g must run exactly 2 at once, peak %d", got)
	}
}

// Concurrent builds never share a builder (a cgroup): each admitted service holds a distinct slot, and
// the lowest free one is taken so serial builds reuse slot 0's cache.
func TestConcurrentBuildsGetDistinctSlotBuilders(t *testing.T) {
	e := newRunEngine(t, Config{FastLane: 4, SlowLane: 4, MemoryCeiling: 12 * gib, SlotMemory: 4 * gib})
	var mu sync.Mutex
	seen := map[string]int{}
	cur, peak := 0, 0
	e.execBuildx = fakeBuildx(func(_ ServiceSpec, args []string) {
		var name string
		for i, a := range args {
			if a == "--builder" {
				name = args[i+1]
			}
		}
		mu.Lock()
		seen[name]++
		cur++
		peak = max(peak, cur)
		if seen[name] > 1 && cur > 1 {
			t.Errorf("builder %s used by two concurrent builds", name)
		}
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		cur--
		seen[name]--
		mu.Unlock()
	})
	d := Descriptor{Project: "p", Services: []ServiceSpec{
		{Name: "a", Dockerfile: "Dockerfile"}, {Name: "b", Dockerfile: "Dockerfile"}, {Name: "c", Dockerfile: "Dockerfile"}}}
	runJob(t, e, d, BuildRequest{})
	if peak != 3 {
		t.Fatalf("12g/4g = 3 slots, peak %d", peak)
	}
	p := e.sched.slots
	_, r0, _ := p.acquire(context.Background())
	b1, r1, _ := p.acquire(context.Background())
	r0()
	b0, r0b, _ := p.acquire(context.Background())
	if b1.name != "b-s1" || b0.name != "b" {
		t.Fatalf("lowest free slot first: got %s then %s", b1.name, b0.name)
	}
	r1()
	r0b()
}

// A build waiting for a slot is FIFO-queued, not refused, and a cancelled waiter frees its place.
func TestSlotPoolQueuesAndCancels(t *testing.T) {
	p := newSlotPool(Config{Builder: "b", MemoryCeiling: 4 * gib, SlotMemory: 4 * gib})
	_, rel, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := p.acquire(ctx); err == nil {
		t.Fatal("second acquire on a 1-slot pool must wait, then fail with the context")
	}
	rel()
	if _, rel2, err := p.acquire(context.Background()); err != nil {
		t.Fatalf("slot not returned after the cancelled waiter: %v", err)
	} else {
		rel2()
	}
}

func TestSchedulerWiringIsOnePerEngine(t *testing.T) {
	e := newEngine(Config{FastLane: 3, SlowLane: 2, MemoryCeiling: 2 * gib, SlotMemory: gib}, nil)
	if e.sched == nil || e.sched.fast.size != 3 || e.sched.slow.size != 2 {
		t.Fatal("engine must own one agent-wide scheduler sized from config")
	}
	src, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "sem <- struct{}{}") || strings.Contains(string(src), "laneSem") {
		t.Fatal("engine.go creates a per-job channel semaphore again")
	}
}
