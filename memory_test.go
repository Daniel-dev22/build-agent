package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseBytes(t *testing.T) {
	ok := map[string]int64{
		"": 0, "0": 0, "1024": 1024, "512m": 512 << 20, "512M": 512 << 20, "6g": 6 << 30, "6GiB": 6 << 30,
		"6gb": 6 << 30, " 2g ": 2 << 30, "64k": 64 << 10, "1t": 1 << 40,
	}
	for in, want := range ok {
		if got, err := parseBytes(in); err != nil || got != want {
			t.Errorf("parseBytes(%q)=%d,%v want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"g", "-1g", "1.5g", "six", "6x", "0x10"} {
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
	none := strings.Join(builderCreateArgs(Config{Builder: "b"}), " ")
	if strings.Contains(none, "memory") {
		t.Errorf("no ceiling configured must not add a cap (previous behaviour): %q", none)
	}
}

// dockerLog records calls and answers builder inspect with a configurable cap.
type dockerLog struct {
	calls       []string
	builderMem  string // "mem swap" for `docker inspect`; "" = builder absent
	rmKeepState bool
}

func (l *dockerLog) fn(_ context.Context, args ...string) (string, error) {
	l.calls = append(l.calls, strings.Join(args, " "))
	switch {
	case len(args) >= 2 && args[0] == "buildx" && args[1] == "inspect":
		if l.builderMem == "" {
			return "", errors.New("no builder")
		}
		return "ok", nil
	case args[0] == "inspect":
		return l.builderMem, nil
	case len(args) >= 3 && args[1] == "rm" && args[2] == "--keep-state":
		l.rmKeepState = true
	}
	return "", nil
}

func (l *dockerLog) did(prefix string) bool {
	for _, c := range l.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestReconcileRecreatesAnUncappedBuilderKeepingState(t *testing.T) {
	l := &dockerLog{builderMem: "0 0"}
	e := &engine{cfg: Config{Builder: "b", MemoryCeiling: 7 << 30}, docker: l.fn}
	if err := e.reconcileBuilder(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !l.rmKeepState {
		t.Fatalf("an uncapped builder must be removed with --keep-state; calls=%v", l.calls)
	}
	for _, c := range l.calls {
		if strings.Contains(c, "rm --force") || strings.Contains(c, "rm b --force") {
			t.Fatalf("--force would delete the BuildKit state volume (the Gradle cache): %q", c)
		}
	}
}

func TestReconcileLeavesACorrectlyCappedBuilderAlone(t *testing.T) {
	l := &dockerLog{builderMem: "7516192768 7516192768"}
	e := &engine{cfg: Config{Builder: "b", MemoryCeiling: 7 << 30}, docker: l.fn}
	if err := e.reconcileBuilder(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.did("buildx rm") {
		t.Fatalf("a builder that already carries the cap must not be touched: %v", l.calls)
	}
}

func TestReconcileIsANoOpWithoutACeiling(t *testing.T) {
	l := &dockerLog{builderMem: "0 0"}
	e := &engine{cfg: Config{Builder: "b"}, docker: l.fn}
	if err := e.reconcileBuilder(context.Background()); err != nil || len(l.calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, l.calls)
	}
}

func TestEnsureBuilderRefusesAnUncappedBuilderWhenACeilingIsSet(t *testing.T) {
	l := &dockerLog{builderMem: "0 0"}
	e := &engine{cfg: Config{Builder: "b", MemoryCeiling: 7 << 30}, docker: l.fn}
	err := e.ensureBuilder(context.Background(), newJob(t))
	if err == nil || !strings.Contains(err.Error(), "BUILD_MEMORY_CEILING") {
		t.Fatalf("an uncapped builder would let a build exhaust the host; want a clear refusal, got %v", err)
	}
	if l.did("buildx rm") || l.did("buildx create") {
		t.Fatalf("ensureBuilder must never recreate under load (it would kill running builds): %v", l.calls)
	}
}

func TestEnsureBuilderCreatesCappedWhenMissingAndNeverForceRemoves(t *testing.T) {
	l := &dockerLog{}
	e := &engine{cfg: Config{Builder: "b", MemoryCeiling: 7 << 30}, docker: l.fn}
	if err := e.ensureBuilder(context.Background(), newJob(t)); err != nil {
		t.Fatal(err)
	}
	var create string
	for _, c := range l.calls {
		if strings.HasPrefix(c, "buildx create") {
			create = c
		}
		if strings.Contains(c, "--force") {
			t.Fatalf("a missing builder must not be force-removed (state volume): %q", c)
		}
	}
	if !strings.Contains(create, "memory=7516192768") || !strings.Contains(create, "memory-swap=7516192768") {
		t.Fatalf("created without the cap: %q", create)
	}
}

func TestEnsureBuilderReusesACappedBuilder(t *testing.T) {
	t.Setenv("CC_RECREATE_BUILDER", "")
	l := &dockerLog{builderMem: "7516192768 7516192768"}
	e := &engine{cfg: Config{Builder: "b", MemoryCeiling: 7 << 30}, docker: l.fn}
	if err := e.ensureBuilder(context.Background(), newJob(t)); err != nil {
		t.Fatal(err)
	}
	if l.did("buildx create") {
		t.Fatalf("reuse expected: %v", l.calls)
	}
}
