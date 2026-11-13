package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Memory caps.
//
// BuildKit runs as ONE persistent docker-container builder, so its cgroup limit is fixed when the
// builder CONTAINER is created: there is no per-job cgroup. That decides the whole design:
//
//   - the OPERATOR sets BUILD_MEMORY_CEILING; the builder container is created with memory =
//     memory-swap = ceiling, a hard cap with no swap. This is the only real enforcement.
//     Sizing rule (deploy config, not code): ceiling = floor_0.5GiB(p1 MemAvailable over >= 7 days
//     with the builder idle - max(1.5 GiB, 20%)), then size the heaviest build (Gradle -Xmx, workers)
//     so its measured memory.peak <= ceiling - 0.5g. Never raise the ceiling to make a build fit.
//   - a service's manifest `memory` is a RESERVATION, not a cgroup setting. The scheduler admits a
//     service only while the reservations of running services fit under the ceiling. It is honour
//     system: declare `memory` on every heavy service, the default only charges BUILD_MEMORY_DEFAULT.
//   - a manifest value above the ceiling is rejected with a clear error.
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

// checkMemory validates every service's `memory` against the operator ceiling. ceiling <= 0 means no
// ceiling is configured: values are syntax-checked but not enforced.
func (d Descriptor) checkMemory(ceiling int64) error {
	for _, s := range d.Services {
		n, err := parseBytes(s.Memory)
		if err != nil {
			return fmt.Errorf("service %s: memory: %w", s.Name, err)
		}
		if ceiling > 0 && n > ceiling {
			return fmt.Errorf("service %s: memory %s exceeds the agent's BUILD_MEMORY_CEILING (%d bytes); lower it or ask the operator to raise the ceiling", s.Name, s.Memory, ceiling)
		}
	}
	return nil
}

// builderCreateArgs assembles `docker buildx create`. Pure so the cap flags are testable.
func builderCreateArgs(cfg Config) []string {
	args := []string{"buildx", "create", "--name", cfg.Builder,
		"--driver", "docker-container", "--driver-opt", "network=host"}
	if cfg.MemoryCeiling > 0 {
		m := strconv.FormatInt(cfg.MemoryCeiling, 10)
		args = append(args, "--driver-opt", "memory="+m, "--driver-opt", "memory-swap="+m)
	}
	args = append(args, "--bootstrap")
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
	if e.docker != nil {
		return e.docker(ctx, args...)
	}
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// containerState reports whether the builder container exists and whether it carries the cap.
// Memory is the enforcing field; MemorySwap is accepted as the ceiling, 0 or -1, because a daemon
// without swap-limit support drops it, and refusing every build over that would let the guard
// disable the function it protects.
func (e *engine) containerState(ctx context.Context) (exists, capped bool, err error) {
	out, ierr := e.dockerRun(ctx, "inspect", "--format", "{{.HostConfig.Memory}} {{.HostConfig.MemorySwap}}", builderContainer(e.cfg.Builder))
	if ierr != nil {
		if strings.Contains(strings.ToLower(out), "no such") {
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
	c := e.cfg.MemoryCeiling
	return true, mem == c && (swap == c || swap == 0 || swap == -1), nil
}

// repairBuilder makes the builder container carry the ceiling. Only the CONTAINER is removed: its
// named state volume (the Gradle/Go cache mounts) survives and the recreate reuses it by name. The
// caller guarantees nothing is building (startup, or builderMu held with zero running services).
func (e *engine) repairBuilder(ctx context.Context) error {
	if out, err := e.dockerRun(ctx, "rm", "-f", builderContainer(e.cfg.Builder)); err != nil && !strings.Contains(strings.ToLower(out), "no such") {
		return fmt.Errorf("remove uncapped builder container: %w: %s", err, out)
	}
	// Drop the (possibly stale) store entry so `buildx create` of the same name is accepted. No
	// --force and no volume removal: --keep-state.
	_, _ = e.dockerRun(ctx, "buildx", "rm", "--keep-state", e.cfg.Builder)
	return nil
}

// reconcileBuilder runs once at startup, before any job is accepted.
func (e *engine) reconcileBuilder(ctx context.Context) error {
	if e.cfg.MemoryCeiling <= 0 {
		return nil
	}
	exists, capped, err := e.containerState(ctx)
	if err != nil {
		return err
	}
	if exists && !capped {
		return e.repairBuilder(ctx)
	}
	return nil
}
