package main

import (
	"strings"
	"testing"
)

func TestRenderBuildArgsTemplating(t *testing.T) {
	d := Descriptor{
		ImagePrefix: "controller-",
		BuildArgs:   map[string]string{"APK_CACHE_BUST": "${APK_CACHE_BUST}", "SERVICE_DIR": "${SERVICE_DIR}"},
	}
	svc := ServiceSpec{Name: "mock-publisher", Dockerfile: "tools/mock-publisher/Dockerfile",
		BuildArgs: map[string]string{"X": "v=${VERSION}"}}
	got := strings.Join(renderBuildArgs(d, svc, "20260101-000000", "2026-22"), " ")
	for _, want := range []string{
		"APK_CACHE_BUST=2026-22",
		"SERVICE_DIR=tools/mock-publisher",
		"X=v=20260101-000000",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered args missing %q in %q", want, got)
		}
	}
}

func TestSelectServices(t *testing.T) {
	d := Descriptor{Services: []ServiceSpec{{Name: "router"}, {Name: "frontend"}, {Name: "log-api"}}}
	if all := selectServices(d, nil); len(all) != 3 {
		t.Fatalf("empty subset => all; got %d", len(all))
	}
	sub := selectServices(d, []string{"frontend", "log-api"})
	if len(sub) != 2 || sub[0].Name != "frontend" || sub[1].Name != "log-api" {
		t.Fatalf("subset preserves descriptor order; got %+v", sub)
	}
}

func TestPlatformArgs(t *testing.T) {
	auto := platformArgs(Descriptor{Platform: PlatformSpec{Mode: "auto"}})
	if strings.Join(auto, " ") != "--platform linux/amd64,linux/arm64" {
		t.Errorf("auto => multi-arch; got %v", auto)
	}
	fixed := platformArgs(Descriptor{Platform: PlatformSpec{Mode: "fixed", Value: "linux/arm64"}})
	if strings.Join(fixed, " ") != "--platform linux/arm64" {
		t.Errorf("fixed wins; got %v", fixed)
	}
}

func TestBuildxArgsTarget(t *testing.T) {
	d := Descriptor{Platform: PlatformSpec{Mode: "fixed", Value: "linux/amd64"}}
	// With a target: --target must appear, before the trailing -t/-f/--push tail.
	svc := ServiceSpec{Name: "frigate", Context: ".", Dockerfile: "docker/main/Dockerfile", Target: "frigate"}
	args := buildxArgs("cc-builder", d, svc, "reg/frigate", "docker/main/Dockerfile", "20260101-000000", "2026-01", "")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--target frigate") {
		t.Errorf("expected --target frigate; got %q", joined)
	}
	if !strings.Contains(joined, "--platform linux/amd64") {
		t.Errorf("expected fixed platform; got %q", joined)
	}
	ti := strings.Index(joined, "--target")
	pi := strings.Index(joined, "--push")
	if ti < 0 || pi < 0 || ti > pi {
		t.Errorf("--target must precede the --push tail; got %q", joined)
	}
	// Without a target: no --target flag at all.
	none := buildxArgs("cc-builder", d, ServiceSpec{Name: "x", Dockerfile: "x/Dockerfile"}, "reg/x", "x/Dockerfile", "v", "a", "")
	if strings.Contains(strings.Join(none, " "), "--target") {
		t.Errorf("no Target => no --target flag; got %q", strings.Join(none, " "))
	}
}

func TestVersionTagFor(t *testing.T) {
	ext := versionTagFor(Descriptor{Version: VersionSpec{Scheme: "external"}}, BuildRequest{Version: "v3.2.1"})
	if ext != "v3.2.1" {
		t.Errorf("external uses --version; got %q", ext)
	}
	ts := versionTagFor(Descriptor{Version: VersionSpec{Scheme: "timestamp"}}, BuildRequest{})
	if !tsTagRe.MatchString(ts) {
		t.Errorf("timestamp scheme => YYYYMMDD-HHMMSS; got %q", ts)
	}
}

func TestRepoSlugAndTokenURL(t *testing.T) {
	cases := map[string]string{
		"git@github.com:Daniel-dev22/agent-kit-go.git":       "Daniel-dev22/agent-kit-go",
		"https://github.com/Daniel-dev22/controller.git": "Daniel-dev22/controller",
		"https://github.com/Daniel-dev22/example-service":    "Daniel-dev22/example-service",
	}
	for in, want := range cases {
		if got := repoSlug(in); got != want {
			t.Errorf("repoSlug(%q)=%q want %q", in, got, want)
		}
	}
	url := githubTokenURL("git@github.com:Daniel-dev22/agent-kit-go.git", "TOK")
	if url != "https://x-access-token:TOK@github.com/Daniel-dev22/agent-kit-go.git" {
		t.Errorf("token url: %q", url)
	}
}

func TestParseBuildxStep(t *testing.T) {
	cases := []struct {
		line             string
		wantOK           bool
		wantCur, wantTot int
		wantName         string
	}{
		{"#12 [router 4/9] RUN go build ./...", true, 4, 9, "RUN go build ./..."},
		{"#8 [linux/amd64 builder 3/6] COPY . .", true, 3, 6, "COPY . ."},
		{"#5 [stage-1 2/2] WORKDIR /app", true, 2, 2, "WORKDIR /app"},
		// stage label only (no trailing command) falls back to the label.
		{"#7 [builder 1/6]", true, 1, 6, "builder"},
		// non-step lines must not match.
		{"#12 0.521 go: downloading ...", false, 0, 0, ""},
		{"#12 DONE 2.3s", false, 0, 0, ""},
		{"#9 CACHED", false, 0, 0, ""},
		{"#1 [internal] load build definition from Dockerfile", false, 0, 0, ""},
		{"random log output", false, 0, 0, ""},
		{"", false, 0, 0, ""},
	}
	for _, c := range cases {
		cur, tot, name, ok := parseBuildxStep(c.line)
		if ok != c.wantOK {
			t.Errorf("parseBuildxStep(%q) ok=%v want %v", c.line, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if cur != c.wantCur || tot != c.wantTot || name != c.wantName {
			t.Errorf("parseBuildxStep(%q) = (%d,%d,%q) want (%d,%d,%q)",
				c.line, cur, tot, name, c.wantCur, c.wantTot, c.wantName)
		}
	}
}

// TestParseBuildxStepBounds verifies a very long RUN line is truncated.
func TestParseBuildxStepBounds(t *testing.T) {
	long := "RUN " + strings.Repeat("x", 200)
	_, _, name, ok := parseBuildxStep("#3 [builder 2/5] " + long)
	if !ok {
		t.Fatal("expected match")
	}
	if len(name) != 80 {
		t.Errorf("step name not bounded: len=%d", len(name))
	}
}

func TestIsPrivate(t *testing.T) {
	yes := true
	no := false
	if !isPrivate(BuildRequest{Repo: "git@github.com:Daniel-dev22/x.git"}) {
		t.Error("org repo should infer private")
	}
	if isPrivate(BuildRequest{Repo: "https://github.com/public/x", Private: &no}) {
		t.Error("explicit private=false wins")
	}
	if !isPrivate(BuildRequest{Repo: "https://github.com/public/x", Private: &yes}) {
		t.Error("explicit private=true wins")
	}
}
