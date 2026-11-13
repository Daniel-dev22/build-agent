package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
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

func TestCheckMemoryRejectsOverCeilingAndGarbage(t *testing.T) {
	const g = int64(1) << 30
	d := Descriptor{Services: []ServiceSpec{{Name: "android", Memory: "8g"}, {Name: "go"}}}
	err := d.checkMemory(7 * g)
	if err == nil || !strings.Contains(err.Error(), "android") || !strings.Contains(err.Error(), "BUILD_MEMORY_CEILING") {
		t.Fatalf("an 8g manifest under a 7g ceiling must be rejected naming the service and the knob: %v", err)
	}
	if err := d.checkMemory(8 * g); err != nil {
		t.Fatalf("exactly the ceiling is allowed: %v", err)
	}
	if err := d.checkMemory(0); err != nil {
		t.Fatalf("no ceiling configured: syntax only, got %v", err)
	}
	if err := (Descriptor{Services: []ServiceSpec{{Name: "x", Memory: "lots"}}}).checkMemory(g); err == nil {
		t.Fatal("unparseable memory must be rejected")
	}
}

func TestBuilderCreateArgsCapMemoryAndSwapEqually(t *testing.T) {
	cfg := Config{Builder: "b", MemoryCeiling: 7 << 30}
	joined := strings.Join(builderCreateArgs(cfg), " ")
	for _, want := range []string{"--driver-opt memory=7516192768", "--driver-opt memory-swap=7516192768", "--driver-opt network=host", "--bootstrap"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %q", want, joined)
		}
	}
	if strings.Contains(strings.Join(builderCreateArgs(Config{Builder: "b"}), " "), "memory") {
		t.Error("no ceiling configured must not add a cap (previous behaviour)")
	}
}

// fakeHost models the docker host the way production behaves: the buildx STORE (in the agent
// container) and the builder CONTAINER (on the host daemon) are independent, and `buildx create`
// over an existing container only starts it, discarding the --driver-opt limits.
type fakeHost struct {
	storeOK, exists bool
	mem, swap       int64
	creates         int
	forceRm         int
	volumeRm        int
	calls           []string
	dropSwap        bool // daemon without swap-limit support: swap reads back -1
	createIgnoresCap bool // buildx create never applies the opts (even on a fresh container)
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
	case args[0] == "inspect":
		if !h.exists {
			return "Error: No such object: x", errors.New("exit 1")
		}
		swap := h.swap
		if h.dropSwap {
			swap = -1
		}
		return strconv.FormatInt(h.mem, 10) + " " + strconv.FormatInt(swap, 10), nil
	case args[0] == "rm" && args[1] == "-f":
		h.exists = false
	case args[0] == "volume" && args[1] == "rm":
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

func hostEngine(h *fakeHost) *engine {
	e := &engine{cfg: Config{Builder: "b", MemoryCeiling: 7 << 30}, docker: h.fn}
	e.sched = newScheduler(e.cfg)
	return e
}

// The deploy scenario: the agent container was recreated (store empty) while the uncapped builder
// container survives on the host. create over it would keep it uncapped.
func TestStoreLostButUncappedContainerSurvivingIsRepairedNotTrusted(t *testing.T) {
	h := &fakeHost{storeOK: false, exists: true}
	e := hostEngine(h)
	if err := e.ensureBuilder(context.Background(), newJob(t)); err != nil {
		t.Fatal(err)
	}
	if h.mem != 7<<30 {
		t.Fatalf("builder still uncapped after ensureBuilder: mem=%d calls=%v", h.mem, h.calls)
	}
	if h.forceRm != 0 || h.did("rm -f -v") || h.did(" -v ") {
		t.Fatalf("repair must remove the container only, never the state volume: %v", h.calls)
	}
	// And the next job reuses it: no refusal loop.
	before := h.creates
	if err := e.ensureBuilder(context.Background(), newJob(t)); err != nil || h.creates != before {
		t.Fatalf("second job: err=%v creates %d->%d", err, before, h.creates)
	}
}

func TestEnsureBuilderVerifiesTheCapAfterCreate(t *testing.T) {
	h := &fakeHost{createIgnoresCap: true}
	err := hostEngine(h).ensureBuilder(context.Background(), newJob(t))
	if err == nil || !strings.Contains(err.Error(), "BUILD_MEMORY_CEILING") {
		t.Fatalf("a create that did not apply the cap must not be trusted: %v", err)
	}
}

func TestEnsureBuilderRepairsInPlaceOnlyWhenIdle(t *testing.T) {
	h := &fakeHost{storeOK: true, exists: true}
	e := hostEngine(h)
	e.sched.running.Add(1)
	if err := e.ensureBuilder(context.Background(), newJob(t)); err == nil {
		t.Fatal("must not recreate the builder under a running build")
	}
	if h.did("rm -f") || h.creates != 0 {
		t.Fatalf("touched the builder while a build was running: %v", h.calls)
	}
	e.sched.running.Add(-1)
	if err := e.ensureBuilder(context.Background(), newJob(t)); err != nil || h.mem != 7<<30 {
		t.Fatalf("idle: repair expected, err=%v mem=%d", err, h.mem)
	}
}

func TestSwapReadbackIsNotRequiredToMatch(t *testing.T) {
	h := &fakeHost{storeOK: true, exists: true, mem: 7 << 30, swap: 7 << 30, dropSwap: true}
	e := hostEngine(h)
	if err := e.ensureBuilder(context.Background(), newJob(t)); err != nil || h.creates != 0 {
		t.Fatalf("a daemon without swap-limit support reads swap back as -1; Memory is the enforcing field: err=%v creates=%d", err, h.creates)
	}
	h.swap, h.dropSwap = 5, false // a WRONG memory must still fail
	h.mem = 1 << 30
	e.sched.running.Add(1)
	if err := e.ensureBuilder(context.Background(), newJob(t)); err == nil {
		t.Fatal("wrong Memory must not pass")
	}
}

func TestReconcileRemovesOnlyAnUncappedContainerAndKeepsState(t *testing.T) {
	h := &fakeHost{exists: true}
	if err := hostEngine(h).reconcileBuilder(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.exists || h.forceRm != 0 {
		t.Fatalf("uncapped container must be removed without --force: %v", h.calls)
	}
	h2 := &fakeHost{exists: true, mem: 7 << 30, swap: 7 << 30}
	_ = hostEngine(h2).reconcileBuilder(context.Background())
	if h2.did("rm -f") || h2.did("buildx rm") {
		t.Fatalf("a capped builder must not be touched: %v", h2.calls)
	}
	h3 := &fakeHost{exists: true}
	e3 := hostEngine(h3)
	e3.cfg.MemoryCeiling = 0
	_ = e3.reconcileBuilder(context.Background())
	if len(h3.calls) != 0 {
		t.Fatalf("no ceiling: no-op, got %v", h3.calls)
	}
}

func TestEnsureBuilderCreatesCappedWhenMissing(t *testing.T) {
	h := &fakeHost{}
	if err := hostEngine(h).ensureBuilder(context.Background(), newJob(t)); err != nil || h.mem != 7<<30 || h.forceRm != 0 {
		t.Fatalf("err=%v mem=%d force=%d calls=%v", err, h.mem, h.forceRm, h.calls)
	}
}

func TestRecreateEnvIsHonouredOncePerAgentLifetime(t *testing.T) {
	t.Setenv("CC_RECREATE_BUILDER", "1")
	h := &fakeHost{storeOK: true, exists: true, mem: 7 << 30, swap: 7 << 30}
	e := hostEngine(h)
	for i := 0; i < 3; i++ {
		if err := e.ensureBuilder(context.Background(), newJob(t)); err != nil {
			t.Fatal(err)
		}
	}
	if h.volumeRm != 1 {
		t.Fatalf("CC_RECREATE_BUILDER removed the cache volume %d times; want once", h.volumeRm)
	}
}

// Setting CC_RECREATE_BUILDER means recreating the agent container, which EMPTIES the buildx store.
// Keyed on the store the request was a silent no-op (buildx rm fails, container and volume survive,
// create just restarts them). It must act on the container and its state volume.
func TestRecreateEnvActsOnTheContainerWhenTheStoreIsLost(t *testing.T) {
	t.Setenv("CC_RECREATE_BUILDER", "1")
	h := &fakeHost{storeOK: false, exists: true, mem: 7 << 30, swap: 7 << 30}
	e := hostEngine(h)
	if err := e.ensureBuilder(context.Background(), newJob(t)); err != nil {
		t.Fatal(err)
	}
	if h.volumeRm != 1 || !h.did("rm -f buildx_buildkit_b0") || !h.did("volume rm buildx_buildkit_b0_state") {
		t.Fatalf("clean recreate did not remove container + state volume: %v", h.calls)
	}
	if h.creates != 1 || h.mem != 7<<30 {
		t.Fatalf("not recreated capped: creates=%d mem=%d", h.creates, h.mem)
	}
}

func TestRecreateEnvIsDeferredWhileBuildsRun(t *testing.T) {
	t.Setenv("CC_RECREATE_BUILDER", "1")
	h := &fakeHost{storeOK: true, exists: true, mem: 7 << 30, swap: 7 << 30}
	e := hostEngine(h)
	e.ensured.Add(1)
	if err := e.ensureBuilder(context.Background(), newJob(t)); err == nil || h.volumeRm != 0 {
		t.Fatalf("must not delete the cache volume under a job: err=%v vol=%d", err, h.volumeRm)
	}
	e.ensured.Add(-1)
	if err := e.ensureBuilder(context.Background(), newJob(t)); err != nil || h.volumeRm != 1 {
		t.Fatalf("idle: err=%v vol=%d", err, h.volumeRm)
	}
}

// A job that already passed ensureBuilder (e.g. running pre_build) can be admitted any moment; the
// repair must not remove the container under it.
func TestRepairWaitsForJobsThatPassedEnsure(t *testing.T) {
	h := &fakeHost{storeOK: true, exists: true}
	e := hostEngine(h)
	e.ensured.Add(1)
	if err := e.ensureBuilder(context.Background(), newJob(t)); err == nil || h.did("rm -f") {
		t.Fatalf("repair under an ensured job: err=%v calls=%v", err, h.calls)
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

func TestHungSetupFailsItsJobAndReleasesTheBuilderLock(t *testing.T) {
	e := newRunEngine(t, Config{FastLane: 1, SlowLane: 1, SetupTimeout: 150 * time.Millisecond})
	hang := true
	e.docker = func(ctx context.Context, _ ...string) (string, error) {
		if hang {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "", nil
	}
	e.execBuildx = fakeBuildx(nil)
	d := Descriptor{Project: "p", Services: []ServiceSpec{{Name: "a", Dockerfile: "Dockerfile"}}}
	b := runJob(t, e, d, BuildRequest{})
	if st := b.snapshot(); st.State != BuildFailed {
		t.Fatalf("a hung ensureBuilder must fail its job at the setup deadline: %s", st.State)
	}
	hang = false
	b2 := runJob(t, e, d, BuildRequest{})
	if st := b2.snapshot(); st.State != BuildCompleted {
		t.Fatalf("builderMu was not released, next job: %s %s", st.State, st.ErrorMsg)
	}
}
