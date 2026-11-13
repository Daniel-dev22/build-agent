package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Memory caps.
//
// BuildKit runs as ONE persistent docker-container builder, so its cgroup limit is fixed when the
// builder is created: there is no per-job cgroup. That decides the whole design:
//
//   - the OPERATOR sets BUILD_MEMORY_CEILING; the builder container is created with memory =
//     memory-swap = ceiling, a hard cap with no swap. This is the only real enforcement, and it is
//     what protects the host from the build.
//   - a service's manifest `memory` is a RESERVATION, not a cgroup setting. The scheduler admits a
//     service only while the reservations of running services fit under the ceiling, so an Android
//     build that needs 6-8 GB runs alone instead of being OOM-killed beside others.
//   - a manifest value above the ceiling is rejected with a clear error; a repo-supplied manifest
//     can never grow the budget.

// parseBytes parses a size such as 512m, 6g, 6GiB, 1048576. Units are binary. "" is 0 (unset).
func parseBytes(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, nil
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "b"), "i")
	mult := int64(1)
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'k':
			mult, s = 1<<10, s[:n-1]
		case 'm':
			mult, s = 1<<20, s[:n-1]
		case 'g':
			mult, s = 1<<30, s[:n-1]
		case 't':
			mult, s = 1<<40, s[:n-1]
		}
	}
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid size %q (want e.g. 512m, 6g)", s)
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

// dockerOut runs docker and returns its trimmed combined output. A field so tests substitute it.
func (e *engine) dockerRun(ctx context.Context, args ...string) (string, error) {
	if e.docker != nil {
		return e.docker(ctx, args...)
	}
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// builderCapped reports whether the existing builder container already has memory = memory-swap =
// the ceiling (what builderCreateArgs would set).
func (e *engine) builderCapped(ctx context.Context) (bool, error) {
	out, err := e.dockerRun(ctx, "inspect", "--format", "{{.HostConfig.Memory}} {{.HostConfig.MemorySwap}}", builderContainer(e.cfg.Builder))
	if err != nil {
		return false, fmt.Errorf("inspect builder container: %w: %s", err, out)
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return false, fmt.Errorf("unexpected inspect output %q", out)
	}
	mem, err1 := strconv.ParseInt(f[0], 10, 64)
	swap, err2 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil {
		return false, fmt.Errorf("unexpected inspect output %q", out)
	}
	return mem == e.cfg.MemoryCeiling && swap == e.cfg.MemoryCeiling, nil
}

// reconcileBuilder runs once at startup, before any job is accepted. An existing builder keeps the
// limits it was created with, so a newly configured ceiling would otherwise never reach it. The
// builder is removed with --keep-state so the BuildKit state volume (the Gradle/Go cache mounts)
// survives the recreate; a plain `rm --force` would delete it. Only safe here: nothing is building.
func (e *engine) reconcileBuilder(ctx context.Context) error {
	if e.cfg.MemoryCeiling <= 0 {
		return nil
	}
	if _, err := e.dockerRun(ctx, "buildx", "inspect", e.cfg.Builder); err != nil {
		return nil // no builder yet: ensureBuilder creates it capped
	}
	capped, err := e.builderCapped(ctx)
	if err == nil && capped {
		return nil
	}
	if out, rerr := e.dockerRun(ctx, "buildx", "rm", "--keep-state", e.cfg.Builder); rerr != nil {
		return fmt.Errorf("remove uncapped builder: %w: %s", rerr, out)
	}
	return nil
}
