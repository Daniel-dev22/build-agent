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
		"git@github.com:Daniel-dev22/agent-kit-go.git":     "Daniel-dev22/agent-kit-go",
		"https://github.com/Daniel-dev22/controller.git": "Daniel-dev22/controller",
		"https://github.com/Daniel-dev22/example-service":  "Daniel-dev22/example-service",
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
