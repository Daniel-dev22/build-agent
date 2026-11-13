package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseBytes(t *testing.T) {
	ok := map[string]int64{
		"": 0, "0": 0, "1024": 1024, "512m": 512 << 20, "512M": 512 << 20, "6g": 6 << 30, "6GiB": 6 << 30,
		"6gb": 6 << 30, " 2g ": 2 << 30, "64k": 64 << 10, "1t": 1 << 40, "7b": 7,
	}
	for in, want := range ok {
		if got, err := parseBytes(in); err != nil || got != want {
			t.Errorf("parseBytes(%q)=%d,%v want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"g", "-1g", "1.5g", "six", "6x", "0x10", "6i", "6ib", "6gx", "17179869185g", "8589934592g", "9223372036854775807k"} {
		if _, err := parseBytes(bad); err == nil {
			t.Errorf("parseBytes(%q) accepted", bad)
		}
	}
}

func TestCheckMemoryRejectsOverSlotAndGarbage(t *testing.T) {
	d := Descriptor{Services: []ServiceSpec{{Name: "android", Memory: "5g"}, {Name: "go"}}}
	err := d.checkMemory(4 * gib)
	if err == nil || !strings.Contains(err.Error(), "android") || !strings.Contains(err.Error(), "BUILD_SLOT_MEMORY") {
		t.Fatalf("a 5g manifest under a 4g slot must be rejected naming the service and the knob: %v", err)
	}
	if err := d.checkMemory(5 * gib); err != nil {
		t.Fatalf("exactly the slot is allowed: %v", err)
	}
	if err := (Descriptor{Services: []ServiceSpec{{Name: "x", Memory: "lots"}}}).checkMemory(gib); err == nil {
		t.Fatal("unparseable memory must be rejected")
	}
}

func TestValidateMemoryHasNoUncappedMode(t *testing.T) {
	for name, c := range map[string]Config{
		"no ceiling":         {},
		"no slot":            {MemoryCeiling: 8 * gib},
		"slot above ceiling": {MemoryCeiling: 4 * gib, SlotMemory: 8 * gib},
		"unitless typo":      {MemoryCeiling: gib, SlotMemory: 4},
		"below minimum":      {MemoryCeiling: gib, SlotMemory: 64 << 20},
		"negative slot":      {MemoryCeiling: 4 * gib, SlotMemory: -1},
	} {
		if err := c.validateMemory(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	c := Config{MemoryCeiling: 9 * gib, SlotMemory: 4 * gib, FastLane: 4, SlowLane: 4}
	if err := c.validateMemory(); err != nil || c.slotCount() != 2 {
		t.Fatalf("9g/4g: err=%v slots=%d (floor, never over-commit)", err, c.slotCount())
	}
}

// Slots beyond fast+slow lanes can never be occupied: 100 GiB / 256 MiB must not mean 400 builders.
func TestSlotCountIsCappedAtTheLanes(t *testing.T) {
	c := Config{MemoryCeiling: 100 * gib, SlotMemory: 256 << 20, FastLane: 2, SlowLane: 1}
	if err := c.validateMemory(); err != nil || c.slotCount() != 3 {
		t.Fatalf("err=%v slots=%d, want 3 (fast+slow)", err, c.slotCount())
	}
	if n := (Config{MemoryCeiling: gib, SlotMemory: gib}).slotCount(); n != 1 {
		t.Fatalf("one slot, got %d", n)
	}
}

func TestBuilderCreateArgsCapMemoryAndSwapEqually(t *testing.T) {
	cfg := Config{Builder: "b", MemoryCeiling: 8 * gib, SlotMemory: 4 * gib}
	joined := strings.Join(builderCreateArgs(cfg, "b-s1"), " ")
	for _, want := range []string{"--name b-s1", "--driver-opt memory=4294967296", "--driver-opt memory-swap=4294967296", "--driver-opt network=host", "--bootstrap"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %q", want, joined)
		}
	}
	if strings.Contains(joined, "8589934592") {
		t.Error("the cap is the SLOT, never the ceiling")
	}
}

func TestSlotBuilderNames(t *testing.T) {
	if slotBuilderName("b", 0) != "b" || slotBuilderName("b", 2) != "b-s2" {
		t.Fatal("slot 0 must keep the original builder name (its state volume is reused)")
	}
}

// fakeHost models the docker host the way production behaves: the buildx STORE (in the agent
// container) and the builder CONTAINER (on the host daemon) are independent, and `buildx create`
// over an existing container only starts it, discarding the --driver-opt limits.
type fakeHost struct {
	storeOK, exists  bool
	mem, swap        int64
	creates          int
	forceRm          int
	volumeRm         int
	calls            []string
	createIgnoresCap bool // buildx create never applies the opts (even on a fresh container)
	psOut            string
	volOut           string
	volRmFails       map[string]bool
}

func (h *fakeHost) fn(_ context.Context, args ...string) (string, error) {
	line := strings.Join(args, " ")
	h.calls = append(h.calls, line)
	switch {
	case line == "buildx inspect b":
		if h.storeOK {
			return "ok", nil
		}
		return "no builder", errors.New("no builder")
	case args[0] == "ps":
		return h.psOut, nil
	case args[0] == "inspect":
		if !h.exists {
			return "Error: No such object: x", errors.New("exit 1")
		}
		return strconv.FormatInt(h.mem, 10) + " " + strconv.FormatInt(h.swap, 10), nil
	case args[0] == "rm" && args[1] == "-f":
		h.exists = false
	case args[0] == "volume" && args[1] == "ls":
		return h.volOut, nil
	case args[0] == "volume" && args[1] == "rm":
		if h.volRmFails[args[2]] {
			return "boom", errors.New("exit 1")
		}
		h.volumeRm++
	case strings.HasPrefix(line, "buildx rm"):
		// A real daemon: removing a builder that is not in the store fails, and it never touches a
		// container it does not know about (the store-lost-on-redeploy case).
		if !h.storeOK {
			return "ERROR: no builder found", errors.New("exit 1")
		}
		h.storeOK = false
		if strings.Contains(line, "--force") {
			h.forceRm++
			h.exists = false
		}
	case strings.HasPrefix(line, "buildx create"):
		h.creates++
		if h.storeOK {
			return "existing name", errors.New("exists")
		}
		h.storeOK = true
		if !h.exists {
			h.exists = true
			if !h.createIgnoresCap {
				for _, a := range args {
					if strings.HasPrefix(a, "memory=") {
						h.mem, _ = strconv.ParseInt(strings.TrimPrefix(a, "memory="), 10, 64)
					}
					if strings.HasPrefix(a, "memory-swap=") {
						h.swap, _ = strconv.ParseInt(strings.TrimPrefix(a, "memory-swap="), 10, 64)
					}
				}
			}
		}
	}
	return "", nil
}

func (h *fakeHost) did(sub string) bool {
	for _, c := range h.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

const slotCap = 4 << 30

func hostEngine(h *fakeHost) *engine {
	e := &engine{cfg: Config{Builder: "b", MemoryCeiling: 8 << 30, SlotMemory: slotCap}, docker: h.fn}
	e.sched = newScheduler(e.cfg)
	return e
}

func slot0(e *engine) *builder { return e.sched.slots.builders[0] }

// The deploy scenario: the agent container was recreated (store empty) while the uncapped builder
// container survives on the host. create over it would keep it uncapped.
func TestStoreLostButUncappedContainerSurvivingIsRepairedNotTrusted(t *testing.T) {
	h := &fakeHost{storeOK: false, exists: true}
	e := hostEngine(h)
	if err := e.ensureBuilder(context.Background(), newJob(t), slot0(e)); err != nil {
		t.Fatal(err)
	}
	if h.mem != slotCap || h.swap != slotCap {
		t.Fatalf("builder still uncapped after ensureBuilder: mem=%d swap=%d calls=%v", h.mem, h.swap, h.calls)
	}
	if h.forceRm != 0 || h.did("rm -f -v") || h.did(" -v ") {
		t.Fatalf("repair must remove the container only, never the state volume: %v", h.calls)
	}
	// And the next job reuses it: no refusal loop.
	before := h.creates
	if err := e.ensureBuilder(context.Background(), newJob(t), slot0(e)); err != nil || h.creates != before {
		t.Fatalf("second job: err=%v creates %d->%d", err, before, h.creates)
	}
}

func TestEnsureBuilderVerifiesTheCapAfterCreate(t *testing.T) {
	h := &fakeHost{createIgnoresCap: true}
	e := hostEngine(h)
	err := e.ensureBuilder(context.Background(), newJob(t), slot0(e))
	if err == nil || !strings.Contains(err.Error(), "BUILD_SLOT_MEMORY") {
		t.Fatalf("a create that did not apply the cap must not be trusted: %v", err)
	}
}

// An existing builder that carries the OLD (pool-sized) cap is repaired the moment its slot is
// acquired; holding the slot is what makes that safe, so there is no "only when idle" refusal.
func TestUncappedOrOldCapSlotIsRepairedOnAcquire(t *testing.T) {
	h := &fakeHost{storeOK: true, exists: true, mem: 8 << 30, swap: 8 << 30}
	e := hostEngine(h)
	if err := e.ensureBuilder(context.Background(), newJob(t), slot0(e)); err != nil || h.mem != slotCap || h.swap != slotCap {
		t.Fatalf("err=%v mem=%d swap=%d calls=%v", err, h.mem, h.swap, h.calls)
	}
	if h.volumeRm != 0 || h.forceRm != 0 {
		t.Fatalf("repair must keep the state volume: %v", h.calls)
	}
}

// swap = memory is the hard cap: unlimited swap (-1), no swap (0) and a different swap are all refused.
func TestSwapMustEqualMemory(t *testing.T) {
	for _, swap := range []int64{-1, 0, slotCap * 2, slotCap - 1} {
		h := &fakeHost{storeOK: true, exists: true, mem: slotCap, swap: swap}
		e := hostEngine(h)
		_, capped, err := e.containerState(context.Background(), "b")
		if err != nil || capped {
			t.Errorf("swap %d: capped=%v err=%v, must not count as capped", swap, capped, err)
		}
	}
	h := &fakeHost{storeOK: true, exists: true, mem: slotCap, swap: slotCap}
	if _, capped, _ := hostEngine(h).containerState(context.Background(), "b"); !capped {
		t.Fatal("memory = swap = slot is the capped state")
	}
}

func TestEnsureBuilderCreatesCappedWhenMissing(t *testing.T) {
	h := &fakeHost{}
	e := hostEngine(h)
	if err := e.ensureBuilder(context.Background(), newJob(t), slot0(e)); err != nil || h.mem != slotCap || h.forceRm != 0 {
		t.Fatalf("err=%v mem=%d force=%d calls=%v", err, h.mem, h.forceRm, h.calls)
	}
}

func TestRecreateEnvIsHonouredOncePerSlot(t *testing.T) {
	t.Setenv("CC_RECREATE_BUILDER", "1")
	h := &fakeHost{storeOK: true, exists: true, mem: slotCap, swap: slotCap}
	e := hostEngine(h)
	for i := 0; i < 3; i++ {
		if err := e.ensureBuilder(context.Background(), newJob(t), slot0(e)); err != nil {
			t.Fatal(err)
		}
	}
	if h.volumeRm != 1 {
		t.Fatalf("CC_RECREATE_BUILDER removed the cache volume %d times; want once", h.volumeRm)
	}
	// Slot 1 is a different builder with its own once: it must also be honoured.
	var vols []string
	e.docker = func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "volume" {
			vols = append(vols, args[2])
		}
		return h.fn(ctx, args...)
	}
	h.storeOK, h.exists = true, true
	_ = e.ensureBuilder(context.Background(), newJob(t), e.sched.slots.builders[1])
	if len(vols) != 1 || vols[0] != "buildx_buildkit_b-s10_state" {
		t.Fatalf("slot 1 must be recreated once on its own volume, got %v", vols)
	}
}

// Setting CC_RECREATE_BUILDER means recreating the agent container, which EMPTIES the buildx store.
// Keyed on the store the request was a silent no-op (buildx rm fails, container and volume survive,
// create just restarts them). It must act on the container and its state volume.
func TestRecreateEnvActsOnTheContainerWhenTheStoreIsLost(t *testing.T) {
	t.Setenv("CC_RECREATE_BUILDER", "1")
	h := &fakeHost{storeOK: false, exists: true, mem: slotCap, swap: slotCap}
	e := hostEngine(h)
	if err := e.ensureBuilder(context.Background(), newJob(t), slot0(e)); err != nil {
		t.Fatal(err)
	}
	if h.volumeRm != 1 || !h.did("rm -f buildx_buildkit_b0") || !h.did("volume rm buildx_buildkit_b0_state") {
		t.Fatalf("clean recreate did not remove container + state volume: %v", h.calls)
	}
	if h.creates != 1 || h.mem != slotCap {
		t.Fatalf("not recreated capped: creates=%d mem=%d", h.creates, h.mem)
	}
}

// Lowering the slot count must not leave a builder container and an unbounded state volume behind.
func TestReconcileRetiresOnlySurplusSlotsWithTheirVolume(t *testing.T) {
	h := &fakeHost{psOut: "buildx_buildkit_b0\nbuildx_buildkit_b-s10\nbuildx_buildkit_b-s20\nbuildx_buildkit_b-s30\nbuildx_buildkit_b-sx0\nbuildx_buildkit_b-s1-x0\nother"}
	e := hostEngine(h) // 8g / 4g = 2 slots: 0 and 1 stay, 2 and 3 go
	e.cfg.FastLane, e.cfg.SlowLane = 4, 4
	if err := e.reconcileSlots(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.did("rm -f buildx_buildkit_b-s10") || h.did("volume rm buildx_buildkit_b-s10_state") || h.did("rm -f buildx_buildkit_b0") || h.did("volume rm buildx_buildkit_b0_state") {
		t.Fatalf("slots 0 and 1 are in range and must stay: %v", h.calls)
	}
	for _, c := range []string{"rm -f buildx_buildkit_b-s20", "volume rm buildx_buildkit_b-s20_state", "rm -f buildx_buildkit_b-s30", "volume rm buildx_buildkit_b-s30_state"} {
		if !h.did(c) {
			t.Errorf("missing %q in %v", c, h.calls)
		}
	}
	if h.did("b-sx0") || h.did("b-s1-x0") || h.did("other") {
		t.Fatalf("unrelated containers must not be touched: %v", h.calls)
	}
}

// A volume whose container is already gone (half-finished retirement, a container prune) is found by
// the volume listing and removed; one slot failing does not stop the next.
func TestReconcileRemovesOrphanVolumesAndSurvivesAFailingSlot(t *testing.T) {
	h := &fakeHost{
		volOut:     "buildx_buildkit_b-s20_state\nbuildx_buildkit_b-s30_state\nbuildx_buildkit_b-s10_state\nbuildx_buildkit_b0_state",
		volRmFails: map[string]bool{"buildx_buildkit_b-s20_state": true},
	}
	e := hostEngine(h)
	e.cfg.FastLane, e.cfg.SlowLane = 4, 4
	err := e.reconcileSlots(context.Background())
	if err == nil || !strings.Contains(err.Error(), "state volume") {
		t.Fatalf("the failing slot must be reported: %v", err)
	}
	if !h.did("volume rm buildx_buildkit_b-s30_state") {
		t.Fatalf("a failure on slot 2 must not skip slot 3: %v", h.calls)
	}
	if h.did("buildx_buildkit_b-s10_state") || h.did("buildx_buildkit_b0_state") {
		t.Fatalf("in-range volumes must stay: %v", h.calls)
	}
}

// ---- OOM reporting ---------------------------------------------------------------------------------

// oomHost models a slot container: memory.max/memory.events as cgroup v2 shows them inside it, a
// StartedAt that changes when docker restarts it, and docker's sticky OOMKilled flag.
type oomHost struct {
	mu        sync.Mutex
	kills     int64
	execOK    bool   // the container is readable
	maxLine   string // memory.max as the container sees it ("" = the slot cap)
	startedAt string
	running   bool
	oomState  string // reply to the OOMKilled inspect
	runc      bool   // a RUN step is alive
	starts    int
}

func (o *oomHost) fn(_ context.Context, args ...string) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch args[0] {
	case "exec":
		if !o.execOK {
			return "Error: container is not running", errors.New("exit 1")
		}
		if args[2] == "ps" {
			if o.runc {
				return "buildkitd\nbuildkit-runc --log x run --bundle y", nil
			}
			return "buildkitd", nil
		}
		max := o.maxLine
		if max == "" {
			max = strconv.FormatInt(slotCap, 10)
		}
		return fmt.Sprintf("%s\nlow 0\nhigh 0\nmax 5\noom 1\noom_kill %d\noom_group_kill 0", max, o.kills), nil
	case "start":
		o.starts++
		o.running, o.execOK = true, true
		return "", nil
	case "inspect":
		f := strings.Join(args, " ")
		switch {
		case strings.Contains(f, "OOMKilled"):
			return o.oomState, nil
		case strings.Contains(f, "State.Running"):
			if !o.running {
				return "false " + o.startedAt, nil
			}
			return "true " + o.startedAt, nil
		}
		return strconv.FormatInt(slotCap, 10) + " " + strconv.FormatInt(slotCap, 10), nil
	}
	return "", nil
}

func newOOMHost() *oomHost {
	return &oomHost{execOK: true, running: true, startedAt: "2026-11-13T10:00:00Z"}
}

func oomEngine(o *oomHost) *engine {
	e := &engine{cfg: Config{Builder: "b", MemoryCeiling: 8 << 30, SlotMemory: slotCap, FastLane: 2, SlowLane: 1}, docker: o.fn}
	e.sched = newScheduler(e.cfg)
	return e
}

func TestOOMKillCounterDeltaIsReportedAsOOM(t *testing.T) {
	o := newOOMHost()
	o.kills = 3
	e := oomEngine(o)
	before := e.probeSlot(context.Background(), "b")
	if !before.haveKills || before.kills != 3 {
		t.Fatalf("parse oom_kill: %+v", before)
	}
	if oom, _ := e.buildOOMKilled(context.Background(), "b", before, time.Now()); oom {
		t.Fatal("no kill during the build (a plain failure, a cancel, a timeout) is not OOM")
	}
	o.kills = 4
	if oom, restarted := e.buildOOMKilled(context.Background(), "b", before, time.Now()); !oom || restarted {
		t.Fatalf("oom_kill moved during the build: that is an OOM (restarted=%v)", restarted)
	}
}

// The M4 case: buildkitd was the victim and docker restarted the container. oom_kill is back at 0
// (so a delta says "no kill") and OOMKilled is cleared; the new StartedAt is the evidence.
func TestRestartedBuilderDuringABuildIsReportedAsOOM(t *testing.T) {
	o := newOOMHost()
	o.kills = 5
	e := oomEngine(o)
	before := e.probeSlot(context.Background(), "b")
	o.kills, o.startedAt, o.oomState = 0, "2026-11-13T10:07:00Z", "false 0001-01-01T00:00:00Z"
	oom, restarted := e.buildOOMKilled(context.Background(), "b", before, time.Now())
	if !oom || !restarted {
		t.Fatalf("a builder restarted mid-build must read as OOM-suspect: oom=%v restarted=%v", oom, restarted)
	}
	if err := e.oomError(errors.New("x"), true); !strings.Contains(err.Error(), "restarted") {
		t.Fatalf("%v", err)
	}
}

// A stopped container at acquire time is started so its counters can be read (case b).
func TestProbeStartsAStoppedSlotContainer(t *testing.T) {
	o := newOOMHost()
	o.running, o.execOK = false, false
	e := oomEngine(o)
	p := e.probeSlot(context.Background(), "b")
	if o.starts != 1 || !p.haveKills || !p.haveStart {
		t.Fatalf("starts=%d probe=%+v", o.starts, p)
	}
}

// Under a host cgroup namespace or cgroup v1 the files are not the slot's: memory.max does not match,
// the counters are NOT trusted (never a false "no kill"), and docker's own record decides.
func TestForeignCgroupCountersAreNotTrusted(t *testing.T) {
	started := time.Now()
	o := newOOMHost()
	o.maxLine = "max"
	e := oomEngine(o)
	before := e.probeSlot(context.Background(), "b")
	if before.haveKills {
		t.Fatal("memory.max != slot cap: counters must be distrusted")
	}
	o.running = false
	o.oomState = "true " + started.Add(time.Second).UTC().Format(time.RFC3339Nano)
	if oom, _ := e.buildOOMKilled(context.Background(), "b", before, started); !oom {
		t.Fatal("OOMKilled recorded after the build began is an OOM")
	}
	o.oomState = "true " + started.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	if oom, _ := e.buildOOMKilled(context.Background(), "b", before, started); oom {
		t.Fatal("a stale OOMKilled from before this build must not be blamed on it")
	}
	o.oomState = "false 2026-01-01T00:00:00Z"
	if oom, _ := e.buildOOMKilled(context.Background(), "b", before, started); oom {
		t.Fatal("OOMKilled=false is not an OOM")
	}
}

func TestOOMErrorNamesTheLimitAndKeepsTheCause(t *testing.T) {
	e := oomEngine(newOOMHost())
	cause := errors.New("exit status 1")
	err := e.oomError(cause, false)
	if !strings.Contains(err.Error(), "out of memory") || !strings.Contains(err.Error(), "4096MiB") || !errors.Is(err, cause) {
		t.Fatalf("%v", err)
	}
}

// The slot is held until buildkitd has torn the dying solve down (L3), bounded.
func TestWaitIdleHoldsUntilNoStepIsRunningAndIsBounded(t *testing.T) {
	o := newOOMHost()
	o.runc = true
	e := oomEngine(o)
	go func() {
		time.Sleep(300 * time.Millisecond)
		o.mu.Lock()
		o.runc = false
		o.mu.Unlock()
	}()
	start := time.Now()
	e.waitIdle(context.Background(), "b", 5*time.Second)
	if d := time.Since(start); d < 250*time.Millisecond || d > 3*time.Second {
		t.Fatalf("waited %v for a step that ended at 300ms", d)
	}
	o.mu.Lock()
	o.runc = true
	o.mu.Unlock()
	start = time.Now()
	e.waitIdle(context.Background(), "b", 400*time.Millisecond)
	if time.Since(start) > 2*time.Second {
		t.Fatal("waitIdle must give up at its limit")
	}
}

// End to end through buildService: the failing build on an OOM-ing slot surfaces as OOM; the same
// failure with no kill (or a cancelled build) stays a generic error.
func TestBuildServiceReportsOOMOnlyWhenTheSlotCgroupKilled(t *testing.T) {
	d := Descriptor{Project: "p", Services: []ServiceSpec{{Name: "a", Dockerfile: "Dockerfile"}}}
	run := func(kill, restart, cancel bool) error {
		o := newOOMHost()
		e := oomEngine(o)
		ctx, cancelFn := context.WithCancel(context.Background())
		defer cancelFn()
		e.execBuildx = func(context.Context, *Build, ServiceSpec, []string, string, func(string)) error {
			o.mu.Lock()
			if kill {
				o.kills++
			}
			if restart {
				o.kills, o.startedAt = 0, "2026-11-13T11:00:00Z"
			}
			o.mu.Unlock()
			if cancel {
				cancelFn()
			}
			return errors.New("exit code: 137")
		}
		b := newJob(t)
		_ = os.WriteFile(filepath.Join(b.workdir(), "Dockerfile"), []byte("FROM scratch\n"), 0o644)
		_, err := e.buildService(ctx, b, slot0(e), d, d.Services[0], "127.0.0.1:1", "v", "a", "", provenance{}, false, 1, nil, buildExtras{})
		return err
	}
	if err := run(true, false, false); err == nil || !strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("a kill during the build must read as OOM: %v", err)
	}
	if err := run(false, true, false); err == nil || !strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("a restart during the build must read as OOM: %v", err)
	}
	if err := run(false, false, false); err == nil || strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("a plain failure must not read as OOM: %v", err)
	}
	if err := run(true, true, true); err == nil || strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("a cancelled build (also 137) must not read as OOM: %v", err)
	}
}

// ---- GC across slots --------------------------------------------------------------------------------

// GC is derived from docker (slot containers that exist), not from what this process used, and a slot
// a build holds is skipped so a prune never overlaps its create/repair.
func TestBoundedGCPrunesEveryExistingFreeSlotWithSplitBounds(t *testing.T) {
	var mu sync.Mutex
	var prunes []string
	existing := map[string]bool{"buildx_buildkit_b0": true}
	e := newRunEngine(t, Config{FastLane: 2, SlowLane: 1, MemoryCeiling: 8 * gib, SlotMemory: 4 * gib})
	e.docker = func(_ context.Context, args ...string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case args[0] == "inspect":
			if !existing[args[len(args)-1]] {
				return "Error: No such object", errors.New("exit 1")
			}
			return strconv.FormatInt(e.cfg.SlotMemory, 10) + " " + strconv.FormatInt(e.cfg.SlotMemory, 10), nil
		case args[0] == "buildx" && args[1] == "prune":
			prunes = append(prunes, strings.Join(args, " "))
		}
		return "", nil
	}
	// A freshly restarted agent: nothing has been ensured in this process, yet slot 0 exists.
	e.boundedGC(context.Background())
	if len(prunes) != 1 || !strings.Contains(prunes[0], "--builder b ") {
		t.Fatalf("an existing slot is pruned after a restart; a missing one is not: %v", prunes)
	}
	existing["buildx_buildkit_b-s10"] = true
	prunes = nil
	_, rel, _ := e.sched.slots.tryAcquireIdx(0) // a build holds slot 0
	e.boundedGC(context.Background())
	if len(prunes) != 1 || !strings.Contains(prunes[0], "--builder b-s1 ") {
		t.Fatalf("a held slot must be skipped: %v", prunes)
	}
	rel()
	prunes = nil
	e.boundedGC(context.Background())
	if len(prunes) != 2 {
		t.Fatalf("every existing free slot: %v", prunes)
	}
	for _, p := range prunes {
		if !strings.Contains(p, "--max-used-space "+strconv.FormatInt(20<<30, 10)) || !strings.Contains(p, "--reserved-space "+strconv.FormatInt(5<<30, 10)) {
			t.Fatalf("the 40GB/10GB totals must be split across 2 slots: %s", p)
		}
	}
	if _, _, ok := e.sched.slots.tryAcquireIdx(0); !ok {
		t.Fatal("GC must release the slot it took")
	}
}

// ensureBuilder is only ever called by the holder of a slot: nothing may create/repair/recreate a
// builder before admission has granted the slot (this replaces the old "only when idle" tests).
func TestBuilderIsEnsuredOnlyWhileTheSlotIsHeld(t *testing.T) {
	e := newRunEngine(t, Config{FastLane: 2, SlowLane: 1, MemoryCeiling: 8 * gib, SlotMemory: 4 * gib})
	var mu sync.Mutex
	var heldAtEnsure []bool
	inner := fakeDocker(e.cfg)
	e.docker = func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "buildx" && args[1] == "inspect" {
			e.sched.slots.mu.Lock()
			held := e.sched.slots.busy[0] || e.sched.slots.busy[1]
			e.sched.slots.mu.Unlock()
			mu.Lock()
			heldAtEnsure = append(heldAtEnsure, held)
			mu.Unlock()
		}
		return inner(ctx, args...)
	}
	e.execBuildx = fakeBuildx(nil)
	d := Descriptor{Project: "p", Services: []ServiceSpec{{Name: "a", Dockerfile: "Dockerfile"}, {Name: "b", Dockerfile: "Dockerfile"}}}
	runJob(t, e, d, BuildRequest{})
	if len(heldAtEnsure) != 2 {
		t.Fatalf("one ensure per service, got %d", len(heldAtEnsure))
	}
	for _, h := range heldAtEnsure {
		if !h {
			t.Fatal("ensureBuilder ran without a held slot")
		}
	}
}

// ---- MED-1: every docker call and the setup phase are bounded --------------------------------------

func TestDockerCallsHaveTheirOwnTimeout(t *testing.T) {
	e := &engine{cfg: Config{Builder: "b", DockerTimeout: 50 * time.Millisecond}}
	e.docker = func(ctx context.Context, _ ...string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	start := time.Now()
	if _, err := e.dockerRun(context.Background(), "inspect", "x"); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("a wedged daemon must not hang the call: err=%v after %v", err, time.Since(start))
	}
}

func TestHungDockerFailsItsServiceAndReleasesItsSlot(t *testing.T) {
	e := newRunEngine(t, Config{FastLane: 1, SlowLane: 1, DockerTimeout: 150 * time.Millisecond})
	var hang atomic.Bool
	hang.Store(true)
	ok := fakeDocker(e.cfg)
	e.docker = func(ctx context.Context, args ...string) (string, error) {
		if hang.Load() {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return ok(ctx, args...)
	}
	e.execBuildx = fakeBuildx(nil)
	d := Descriptor{Project: "p", Services: []ServiceSpec{{Name: "a", Dockerfile: "Dockerfile"}}}
	b := runJob(t, e, d, BuildRequest{})
	if st := b.snapshot(); st.State != BuildFailed {
		t.Fatalf("a hung ensureBuilder must fail its job at the docker timeout: %s", st.State)
	}
	hang.Store(false)
	b2 := runJob(t, e, d, BuildRequest{})
	if st := b2.snapshot(); st.State != BuildCompleted {
		t.Fatalf("the slot was not released, next job: %s %s", st.State, st.ErrorMsg)
	}
}
