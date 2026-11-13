package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Memory caps.
//
// Every build runs on a SLOT: a persistent BuildKit docker-container builder of its own, created with
// memory = memory-swap = BUILD_SLOT_MEMORY. A container is the only cgroup BuildKit offers (it cannot
// limit one solve, and two buildkitds cannot share a state dir), so one builder per slot is what
// makes the cap per BUILD rather than per pool: a build that outgrows its slot is OOM-killed alone and
// its sibling, in another cgroup, never notices.
//
//   - BUILD_MEMORY_CEILING is the host budget, BUILD_SLOT_MEMORY the per-build cap (default = the
//     ceiling, i.e. one slot). Slots = floor(ceiling / slot), so the caps can never sum above the
//     ceiling. The ceiling is required: there is no uncapped mode.
//   - slot 0 is the original builder (BUILDX_BUILDER), so its container and state volume (the
//     Gradle/Go cache mounts) are reused; slot i is BUILDX_BUILDER-s<i>. The lowest free slot is
//     always taken so serial builds keep hitting the warm cache.
//   - a build holds its slot exclusively from admission to exit, so create / repair / recreate of that
//     slot's builder never races a running build.
//   - a service's manifest `memory` is only validated against the slot cap (it is no reservation: a
//     slot is the unit of isolation).
//
// The decision "is the cap in force?" is made on the builder CONTAINER (docker inspect), never on the
// buildx builder store: the store lives in the agent container's filesystem and is lost on every
// redeploy while the container survives on the host daemon, and `buildx create` over an existing
// container only starts it, silently discarding the --driver-opt limits.

// parseBytes parses a size such as 512m, 6g, 6GiB, 1048576. Units are binary. "" is 0 (unset).
// The only text allowed after the unit is "b" or "ib"; a value that overflows int64 is an error.
func parseBytes(s string) (int64, error) {
	in := strings.ToLower(strings.TrimSpace(s))
	if in == "" {
		return 0, nil
	}
	num, mult := in, int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30}, {"tib", 1 << 40},
		{"kb", 1 << 10}, {"mb", 1 << 20}, {"gb", 1 << 30}, {"tb", 1 << 40},
		{"k", 1 << 10}, {"m", 1 << 20}, {"g", 1 << 30}, {"t", 1 << 40}, {"b", 1}} {
		if strings.HasSuffix(in, u.suffix) {
			num, mult = strings.TrimSuffix(in, u.suffix), u.mult
			break
		}
	}
	v, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid size %q (want e.g. 512m, 6g)", s)
	}
	if v > math.MaxInt64/mult {
		return 0, fmt.Errorf("size %q overflows", s)
	}
	return v * mult, nil
}

// minSlotMemory is the smallest slot that can run a build at all (docker itself refuses < 6 MiB; a
// real build needs far more). It also turns a unitless typo (BUILD_SLOT_MEMORY=4, meaning 4 BYTES)
// into a startup error instead of millions of slots.
const minSlotMemory = 256 << 20

// validateMemory checks the memory knobs. There is deliberately no "unset" branch: a build without a
// hard cap is not a mode this agent has.
func (c Config) validateMemory() error {
	switch {
	case c.MemoryCeiling <= 0:
		return fmt.Errorf("BUILD_MEMORY_CEILING is required (e.g. 8g): the host budget every build slot is carved from")
	case c.SlotMemory < minSlotMemory:
		return fmt.Errorf("BUILD_SLOT_MEMORY (%d bytes) is below the %d-byte minimum; did you forget the unit (e.g. 4g)?", c.SlotMemory, int64(minSlotMemory))
	case c.SlotMemory > c.MemoryCeiling:
		return fmt.Errorf("BUILD_SLOT_MEMORY (%d bytes) exceeds BUILD_MEMORY_CEILING (%d bytes)", c.SlotMemory, c.MemoryCeiling)
	}
	return nil
}

// slotCount is how many builds may run at once: the memory budget allows floor(ceiling / slot), and
// the lanes allow at most fast + slow, so a slot beyond either bound could never be occupied.
func (c Config) slotCount() int {
	if c.SlotMemory <= 0 {
		return 0
	}
	byMemory := c.MemoryCeiling / c.SlotMemory
	byLanes := int64(max(c.FastLane, 1) + max(c.SlowLane, 1))
	return int(min(byMemory, byLanes))
}

// checkMemory validates every service's `memory` against the per-build slot cap.
func (d Descriptor) checkMemory(slot int64) error {
	for _, s := range d.Services {
		n, err := parseBytes(s.Memory)
		if err != nil {
			return fmt.Errorf("service %s: memory: %w", s.Name, err)
		}
		if n > slot {
			return fmt.Errorf("service %s: memory %s exceeds the agent's BUILD_SLOT_MEMORY (%d bytes); lower it or ask the operator to raise the slot size", s.Name, s.Memory, slot)
		}
	}
	return nil
}

// builder is one slot's BuildKit builder. Only the holder of the slot touches the mutable fields, so
// they need no lock.
type builder struct {
	idx       int
	name      string // buildx builder name
	recreated bool   // CC_RECREATE_BUILDER already honoured for this slot
}

func slotBuilderName(base string, idx int) string {
	if idx == 0 {
		return base
	}
	return base + "-s" + strconv.Itoa(idx)
}

// builderCreateArgs assembles `docker buildx create` for one slot. Pure so the cap flags are testable.
func builderCreateArgs(cfg Config, name string) []string {
	m := strconv.FormatInt(cfg.SlotMemory, 10)
	args := []string{"buildx", "create", "--name", name,
		"--driver", "docker-container", "--driver-opt", "network=host",
		"--driver-opt", "memory=" + m, "--driver-opt", "memory-swap=" + m, "--bootstrap"}
	if cfg.BuildkitdConfig != "" {
		if _, err := os.Stat(cfg.BuildkitdConfig); err == nil {
			args = append(args, "--buildkitd-config", cfg.BuildkitdConfig)
		}
	}
	return args
}

// builderContainer is the container buildx names for the builder's single node.
func builderContainer(builder string) string { return "buildx_buildkit_" + builder + "0" }

// dockerRun runs docker and returns its trimmed combined output. e.docker lets tests substitute it.
func (e *engine) dockerRun(ctx context.Context, args ...string) (string, error) {
	if e.cfg.DockerTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.cfg.DockerTimeout)
		defer cancel()
	}
	if e.docker != nil {
		return e.docker(ctx, args...)
	}
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func isNoSuch(out string) bool { return strings.Contains(strings.ToLower(out), "no such") }

// containerState reports whether the slot's container exists and carries the cap. BOTH memory and
// memory-swap must equal the slot: swap = memory is what makes the cap hard, and swap -1 is unlimited
// swap. (Under cgroup v2 swap accounting is always on, so a daemon that drops the swap limit is not
// a case to tolerate.)
func (e *engine) containerState(ctx context.Context, name string) (exists, capped bool, err error) {
	out, ierr := e.dockerRun(ctx, "inspect", "--format", "{{.HostConfig.Memory}} {{.HostConfig.MemorySwap}}", builderContainer(name))
	if ierr != nil {
		if isNoSuch(out) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("inspect builder container: %w: %s", ierr, out)
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return true, false, fmt.Errorf("unexpected inspect output %q", out)
	}
	mem, err1 := strconv.ParseInt(f[0], 10, 64)
	swap, err2 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil {
		return true, false, fmt.Errorf("unexpected inspect output %q", out)
	}
	return true, mem == e.cfg.SlotMemory && swap == e.cfg.SlotMemory, nil
}

// repairBuilder makes the slot's container carry the cap. Only the CONTAINER is removed: its named
// state volume (the Gradle/Go cache mounts) survives and the recreate reuses it by name. The caller
// holds the slot, so nothing is building on it.
func (e *engine) repairBuilder(ctx context.Context, name string) error {
	if out, err := e.dockerRun(ctx, "rm", "-f", builderContainer(name)); err != nil && !isNoSuch(out) {
		return fmt.Errorf("remove uncapped builder container: %w: %s", err, out)
	}
	// Drop the (possibly stale) store entry so `buildx create` of the same name is accepted. No
	// --force and no volume removal: --keep-state.
	_, _ = e.dockerRun(ctx, "buildx", "rm", "--keep-state", name)
	return nil
}

// recreateClean is the operator's CC_RECREATE_BUILDER and the retirement of a surplus slot: a clean
// builder, STATE INCLUDED. Keyed on the container and its named state volume, not on the buildx
// store (which is empty exactly when the env var could be set, since setting it means recreating the
// agent container). Caller guarantees nothing is building on it.
func (e *engine) recreateClean(ctx context.Context, name string) error {
	c := builderContainer(name)
	if out, err := e.dockerRun(ctx, "rm", "-f", c); err != nil && !isNoSuch(out) {
		return fmt.Errorf("remove builder container: %w: %s", err, out)
	}
	if out, err := e.dockerRun(ctx, "volume", "rm", c+"_state"); err != nil && !isNoSuch(out) {
		return fmt.Errorf("remove builder state volume: %w: %s", err, out)
	}
	_, _ = e.dockerRun(ctx, "buildx", "rm", "--keep-state", name)
	return nil
}

// slotRe matches a slot builder's container or state volume; group 1 is the slot index, group 2 is
// "" for the container and "_state" for its volume. buildx appends the node index (0) to the builder
// name, so slot 1 is container ...-s10. The `<BUILDX_BUILDER>-s<n>` name space is RESERVED for this
// agent's slots: another builder on the same daemon must not use it (it would be retired as surplus).
func slotRe(base string) *regexp.Regexp {
	return regexp.MustCompile("^buildx_buildkit_" + regexp.QuoteMeta(base) + `-s(\d+)0(_state)?$`)
}

// reconcileSlots runs once at startup, before any job is accepted: slot builders above the current
// slot count (the operator lowered the ceiling or raised the slot size) are retired with their state
// volume, so a smaller fleet does not leave unbounded cache volumes behind. Containers AND volumes
// are listed, so a volume orphaned by a half-finished retirement is found again; one failing slot does
// not stop the others. Slots in range are left alone; each is verified, and repaired, when a build
// acquires it.
func (e *engine) reconcileSlots(ctx context.Context) error {
	prefix := "buildx_buildkit_" + e.cfg.Builder + "-s"
	ps, err := e.dockerRun(ctx, "ps", "-a", "--filter", "name="+prefix, "--format", "{{.Names}}")
	if err != nil {
		return fmt.Errorf("list slot builders: %w: %s", err, ps)
	}
	vols, err := e.dockerRun(ctx, "volume", "ls", "-q", "--filter", "name="+prefix)
	if err != nil {
		return fmt.Errorf("list slot volumes: %w: %s", err, vols)
	}
	re, n := slotRe(e.cfg.Builder), e.cfg.slotCount()
	surplus := map[int]struct{}{}
	for _, c := range strings.Fields(ps + "\n" + vols) {
		if m := re.FindStringSubmatch(c); m != nil {
			if idx, _ := strconv.Atoi(m[1]); idx >= n {
				surplus[idx] = struct{}{}
			}
		}
	}
	var errs []error
	for idx := range surplus {
		if err := e.recreateClean(ctx, slotBuilderName(e.cfg.Builder, idx)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ---- OOM detection ---------------------------------------------------------------------------------

// slotProbe is what the agent knows about a slot's container at one instant.
type slotProbe struct {
	kills     int64 // oom_kill of the slot's own cgroup (hierarchical: counts a RUN step's kill)
	haveKills bool  // read, and memory.max proved the file is the slot's own cgroup
	startedAt string
	haveStart bool
}

const probeTimeout = 10 * time.Second

// readSlot reads the slot container's state and its cgroup v2 files in one inspect + one exec. The
// container sees its cgroup as /sys/fs/cgroup; memory.max must equal the slot cap, otherwise the files
// belong to a host cgroup namespace or cgroup v1 and the counters are not trusted (detection then
// falls back to docker's own record instead of reporting a wrong "no kill").
func (e *engine) readSlot(ctx context.Context, name string) slotProbe {
	var p slotProbe
	c := builderContainer(name)
	if out, err := e.dockerRun(ctx, "inspect", "--format", "{{.State.Running}} {{.State.StartedAt}}", c); err == nil {
		if f := strings.Fields(out); len(f) == 2 && f[0] == "true" {
			p.startedAt, p.haveStart = f[1], true
		}
	}
	out, err := e.dockerRun(ctx, "exec", c, "cat", "/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory.events")
	if err != nil {
		return p
	}
	lines := strings.Split(out, "\n")
	if strings.TrimSpace(lines[0]) != strconv.FormatInt(e.cfg.SlotMemory, 10) {
		return p
	}
	for _, l := range lines[1:] {
		k, v, ok := strings.Cut(strings.TrimSpace(l), " ")
		n, perr := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if !ok || perr != nil {
			continue
		}
		switch k {
		case "oom_kill":
			p.kills, p.haveKills = n, true
		}
	}
	return p
}

// probeSlot is readSlot for the holder of a freshly acquired slot: a stopped container (an earlier
// kill, a daemon restart) is started first so the counters can be read, and the read has its own
// short timeout so a wedged container cannot eat the build's deadline.
func (e *engine) probeSlot(ctx context.Context, name string) slotProbe {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	p := e.readSlot(ctx, name)
	if !p.haveStart {
		if _, err := e.dockerRun(ctx, "start", builderContainer(name)); err == nil {
			p = e.readSlot(ctx, name)
		}
	}
	return p
}

// buildOOMKilled reports whether the build that just failed on the slot was killed for memory. The
// slot is exclusive, so everything that happened in its cgroup during the build is this build's:
//   - the container restarted (new StartedAt): buildkitd, or the whole container, was killed and
//     docker brought it back, which resets oom_kill and clears OOMKilled. Treated as an OOM kill.
//   - else oom_kill moved: a RUN step was killed.
//   - else (counters unreadable): docker's OOMKilled flag, if it was recorded after the build began.
//
// A cancel or timeout also ends in exit 137 but does none of these, so it is not OOM.
func (e *engine) buildOOMKilled(ctx context.Context, name string, before slotProbe, started time.Time) (oom, restarted bool) {
	after := e.readSlot(ctx, name)
	if before.haveStart && after.haveStart && after.startedAt != before.startedAt {
		return true, true
	}
	if before.haveKills && after.haveKills {
		return after.kills > before.kills, false
	}
	out, err := e.dockerRun(ctx, "inspect", "--format", "{{.State.OOMKilled}} {{.State.FinishedAt}}", builderContainer(name))
	f := strings.Fields(out)
	if err != nil || len(f) != 2 || f[0] != "true" {
		return false, false
	}
	fin, perr := time.Parse(time.RFC3339Nano, f[1])
	return perr == nil && !fin.Before(started), false
}

// stepsRunning reports whether the slot still has a RUN step alive: BuildKit executes each one as a
// `buildkit-runc ... run` child of buildkitd (measured on moby/buildkit: absent when idle, present for
// the whole step). ok is false when the container cannot be asked.
func (e *engine) stepsRunning(ctx context.Context, name string) (running, ok bool) {
	out, err := e.dockerRun(ctx, "exec", builderContainer(name), "ps", "-o", "args")
	if err != nil {
		return false, false
	}
	return strings.Contains(out, "runc"), true
}

// waitIdle holds the slot until buildkitd has torn down the solve that just ended (a cancelled or
// failed buildx client returns before the daemon is done), so the next holder never shares the cgroup
// with a dying RUN step. Bounded; it gives up silently after the limit.
func (e *engine) waitIdle(ctx context.Context, name string, limit time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	for {
		if running, ok := e.stepsRunning(ctx, name); !ok || !running {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// oomError is the failure a build gets when its slot's cgroup killed it.
func (e *engine) oomError(cause error, restarted bool) error {
	how := "killed by the kernel"
	if restarted {
		how = "its builder was killed and restarted, almost certainly by the kernel OOM killer"
	}
	return fmt.Errorf("out of memory: the build exceeded its %dMiB slot limit (memory = memory-swap, %s); reduce its memory use (e.g. Gradle workers, -Xmx) or ask the operator to raise BUILD_SLOT_MEMORY: %w",
		e.cfg.SlotMemory>>20, how, cause)
}
