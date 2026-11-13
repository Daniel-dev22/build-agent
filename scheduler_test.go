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

// fakeDocker answers every docker call as a healthy host whose builder already carries the ceiling.
func fakeDocker(cfg Config) func(context.Context, ...string) (string, error) {
	return func(_ context.Context, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "inspect" {
			c := strconv.FormatInt(cfg.MemoryCeiling, 10)
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

// ---- memory reservations ------------------------------------------------------------------------

// Two services whose reservations do not both fit under the ceiling must run one after the other,
// whatever lane they are in; ones that fit may overlap.
func TestMemoryReservationsAreAdmittedAgainstTheCeiling(t *testing.T) {
	const g = int64(1) << 30
	e := newRunEngine(t, Config{FastLane: 4, SlowLane: 4, MemoryCeiling: 7 * g, MemoryDefault: 1 * g})
	probe := &overlapProbe{}
	e.execBuildx = fakeBuildx(probe.hook(0))
	d := Descriptor{Project: "p", Services: []ServiceSpec{
		{Name: "a", Dockerfile: "Dockerfile", Memory: "5g"},
		{Name: "b", Dockerfile: "Dockerfile", Memory: "5g"}}}
	runJob(t, e, d, BuildRequest{})
	if got := probe.peak.Load(); got != 1 {
		t.Fatalf("5g+5g under a 7g ceiling overlapped (peak %d)", got)
	}

	e2 := newRunEngine(t, Config{FastLane: 4, SlowLane: 4, MemoryCeiling: 7 * g, MemoryDefault: 1 * g})
	probe2 := &overlapProbe{}
	e2.execBuildx = fakeBuildx(probe2.hook(0))
	d2 := Descriptor{Project: "p", Services: []ServiceSpec{
		{Name: "a", Dockerfile: "Dockerfile"}, {Name: "b", Dockerfile: "Dockerfile"}}} // default 1g each
	runJob(t, e2, d2, BuildRequest{})
	if got := probe2.peak.Load(); got != 2 {
		t.Fatalf("two default 1g reservations should overlap under 7g (peak %d)", got)
	}
}

func TestAdmitUnreservedWhenNoCeiling(t *testing.T) {
	s := newScheduler(Config{FastLane: 1, SlowLane: 1})
	if s.weight(ServiceSpec{Memory: "99g"}) != 0 {
		t.Fatal("with no ceiling configured nothing is reserved (previous behaviour)")
	}
}

func TestSchedulerWiringIsOnePerEngine(t *testing.T) {
	e := newEngine(Config{FastLane: 3, SlowLane: 2}, nil)
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
