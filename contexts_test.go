package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// gitFixture makes a local repo with a tag and a second commit, and a global gitconfig that maps
// https://github.com/o/<name>.git onto it, so clones go through the real git binary with no network.
type gitFixture struct {
	root       string
	headSHA    string
	tagSHA     string
	annotSHA   string // the annotated tag OBJECT (peels to tagSHA)
	repoURLFor func(name string) string
}

func newGitFixture(t *testing.T, names ...string) *gitFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	f := &gitFixture{root: root}
	var rules strings.Builder
	for _, n := range names {
		dir := filepath.Join(root, n)
		run(root, "init", "-q", "-b", "main", dir)
		must(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644))
		must(t, os.WriteFile(filepath.Join(dir, "v"), []byte("1"), 0o644))
		run(dir, "add", "-A")
		run(dir, "commit", "-q", "-m", "one")
		run(dir, "tag", "v1")
		run(dir, "config", "uploadpack.allowAnySHA1InWant", "true")
		f.tagSHA = run(dir, "rev-parse", "HEAD")
		run(dir, "tag", "-a", "-m", "x", "v1a")
		f.annotSHA = run(dir, "rev-parse", "v1a")
		must(t, os.WriteFile(filepath.Join(dir, "v"), []byte("2"), 0o644))
		run(dir, "commit", "-q", "-am", "two")
		f.headSHA = run(dir, "rev-parse", "HEAD")
		rules.WriteString("[url \"file://" + dir + "\"]\n\tinsteadOf = https://github.com/o/" + n + ".git\n")
	}
	gcfg := filepath.Join(root, "gitconfig")
	must(t, os.WriteFile(gcfg, []byte(rules.String()), 0o644))
	t.Setenv("GIT_CONFIG_GLOBAL", gcfg)
	f.repoURLFor = func(n string) string { return "https://github.com/o/" + n + ".git" }
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestValidateContexts(t *testing.T) {
	if err := validateContexts(map[string]ContextSpec{"kit": {Repo: "git@github.com:o/kit.git", Ref: "1"}}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]ContextSpec{"Kit": {Repo: "r"}, "k it": {Repo: "r"}, "": {Repo: "r"}, "kit": {Repo: ""}, "ok": {Repo: "-oProxy"}} {
		if err := validateContexts(map[string]ContextSpec{name: c}); err == nil {
			t.Errorf("accepted %q %+v", name, c)
		}
	}
	if err := validateContextRefs(map[string]ContextSpec{"kit": {Repo: "r"}}, map[string]string{"nope": "x"}); err == nil {
		t.Error("a ref override for an undeclared context must be rejected")
	}
}

func TestTokenReposCoversAppAndPrivateContexts(t *testing.T) {
	owners := []string{"Daniel-dev22"}
	d := Descriptor{Contexts: map[string]ContextSpec{
		"kit": {Repo: "git@github.com:Daniel-dev22/android-kit.git"},
		"pub": {Repo: "https://github.com/someone/open.git"},
	}}
	got := tokenRepos(BuildRequest{Repo: "git@github.com:Daniel-dev22/firewatch.git"}, d, owners)
	if strings.Join(got, ",") != "Daniel-dev22/firewatch,Daniel-dev22/android-kit" {
		t.Fatalf("slugs=%v: one token must name every private repo it will clone", got)
	}
	// An uploaded context has no app repo but its private kit still needs a token.
	got = tokenRepos(BuildRequest{ContextB64: "x"}, d, owners)
	if strings.Join(got, ",") != "Daniel-dev22/android-kit" {
		t.Fatalf("upload + private context: %v", got)
	}
	if got := tokenRepos(BuildRequest{Repo: "https://github.com/o/pub.git"}, Descriptor{}, owners); len(got) != 0 {
		t.Fatalf("nothing private, no token: %v", got)
	}
}

func TestBuildExtrasArgs(t *testing.T) {
	x := buildExtras{contexts: []namedContext{{"kit", "/j/ctx-kit"}, {"zz", "/j/ctx-zz"}}, metadataFile: "/j/meta.json"}
	got := strings.Join(x.args(), " ")
	want := "--build-context kit=/j/ctx-kit --build-context zz=/j/ctx-zz --metadata-file /j/meta.json"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if len((buildExtras{}).args()) != 0 {
		t.Fatal("no contexts and no metadata file must add nothing")
	}
}

func TestCloneContextsResolvesRefToShaAndKeepsTokenOut(t *testing.T) {
	fx := newGitFixture(t, "kit")
	e := &engine{cfg: Config{GitTimeout: 30e9}}
	b := newJob(t)
	d := Descriptor{Contexts: map[string]ContextSpec{"kit": {Repo: fx.repoURLFor("kit"), Ref: "v1"}}}

	cs, res, err := e.cloneContexts(context.Background(), b, d, BuildRequest{}, "")
	must(t, err)
	if res["kit"].Revision != fx.tagSHA {
		t.Fatalf("descriptor ref v1 must resolve to the tag commit %s, got %s", fx.tagSHA, res["kit"].Revision)
	}
	if len(cs) != 1 || !strings.HasPrefix(cs[0].dir, b.jobRoot()) || strings.HasPrefix(cs[0].dir, b.workdir()) {
		t.Fatalf("a context must live in the job root BESIDE the app context, not inside it: %+v", cs)
	}

	// The request pins a different ref; the descriptor's is overridden.
	b2 := newJob(t)
	_, res2, err := e.cloneContexts(context.Background(), b2, d, BuildRequest{ContextRefs: map[string]string{"kit": "main"}}, "")
	must(t, err)
	if res2["kit"].Revision != fx.headSHA || res2["kit"].Ref != "main" {
		t.Fatalf("context_refs override ignored: %+v", res2["kit"])
	}
}

func TestCloneContextsFailsTheJobOnAnUnknownRef(t *testing.T) {
	fx := newGitFixture(t, "kit")
	e := &engine{cfg: Config{GitTimeout: 30e9}}
	d := Descriptor{Contexts: map[string]ContextSpec{"kit": {Repo: fx.repoURLFor("kit"), Ref: "no-such-ref"}}}
	if _, _, err := e.cloneContexts(context.Background(), newJob(t), d, BuildRequest{}, ""); err == nil {
		t.Fatal("a context that cannot be cloned at its ref must fail the job, not build without it")
	}
}

// The whole path, through run(): the kit is cloned, its SHA lands in the job result, and every
// service's argv carries --build-context for it.
func TestRunPassesNamedContextsToBuildxAndReportsTheirSha(t *testing.T) {
	fx := newGitFixture(t, "kit")
	e := newRunEngine(t, Config{FastLane: 1, SlowLane: 1})
	var argv []string
	e.execBuildx = fakeBuildx(func(_ ServiceSpec, args []string) { argv = append([]string(nil), args...) })
	d := Descriptor{Project: "p", Services: []ServiceSpec{{Name: "app", Dockerfile: "Dockerfile"}},
		Contexts: map[string]ContextSpec{"kit": {Repo: fx.repoURLFor("kit"), Ref: "v1"}}}
	b := runJob(t, e, d, BuildRequest{})
	snap := b.snapshot()
	if snap.State != BuildCompleted {
		t.Fatalf("state %s: %s\n%s", snap.State, snap.ErrorMsg, strings.Join(b.logLines(), "\n"))
	}
	if snap.Contexts["kit"].Revision != fx.tagSHA {
		t.Fatalf("job result must carry the kit SHA %s, got %+v", fx.tagSHA, snap.Contexts)
	}
	var ctxArg string
	for i, a := range argv {
		if a == "--build-context" && i+1 < len(argv) {
			ctxArg = argv[i+1]
		}
	}
	if !strings.HasPrefix(ctxArg, "kit=") {
		t.Fatalf("argv lacks --build-context kit=<dir>: %v", argv)
	}
}

// ---- per-service platform (item c) ------------------------------------------------------------

// Android pins linux/amd64 on its own service; every other service in the project, and every
// existing build.json without the field, keeps the multi-arch default.
func TestAndroidServicePinsAmd64WhileTheDefaultStaysMultiArch(t *testing.T) {
	d := Descriptor{Services: []ServiceSpec{
		{Name: "firewatch", Platform: &PlatformSpec{Mode: "fixed", Value: "linux/amd64"}},
		{Name: "other"},
	}}
	if err := d.validate(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(platformArgs(d, d.Services[0]), " "); got != "--platform linux/amd64" {
		t.Fatalf("android: %q", got)
	}
	if got := strings.Join(platformArgs(d, d.Services[1]), " "); got != "--platform linux/amd64,linux/arm64" {
		t.Fatalf("default must stay multi-arch: %q", got)
	}
}

// ---- tags, digest, skip_latest (items e, f) ------------------------------------------------------

func TestBuildxArgsPushesBothTagsFromOneBuild(t *testing.T) {
	svc := ServiceSpec{Name: "app", Dockerfile: "Dockerfile"}
	got := strings.Join(buildxArgs("b", Descriptor{}, svc, "reg/app", "Dockerfile", "20260101-000000", "a", "", provenance{}, buildExtras{}), " ")
	if !strings.Contains(got, "-t reg/app:latest -t reg/app:20260101-000000 ") {
		t.Fatalf("both tags must be pushed by the build itself: %q", got)
	}
	skip := ServiceSpec{Name: "aab", Dockerfile: "Dockerfile", SkipLatest: true}
	got = strings.Join(buildxArgs("b", Descriptor{}, skip, "reg/aab", "Dockerfile", "20260101-000000", "a", "", provenance{}, buildExtras{}), " ")
	if strings.Contains(got, ":latest") || !strings.Contains(got, "-t reg/aab:20260101-000000") {
		t.Fatalf("skip_latest must push only the versioned tag: %q", got)
	}
}

func TestRunReportsTagAndDigestAndNeverCopiesFromLatest(t *testing.T) {
	e := newRunEngine(t, Config{FastLane: 1, SlowLane: 1, RetentionKeepDefault: 15})
	var dockerCalls []string
	e.docker = func(_ context.Context, args ...string) (string, error) {
		dockerCalls = append(dockerCalls, strings.Join(args, " "))
		return "", nil
	}
	e.execBuildx = fakeBuildx(nil)
	d := Descriptor{Project: "p", ImagePrefix: "pre-", Services: []ServiceSpec{
		{Name: "aab", Dockerfile: "Dockerfile", SkipLatest: true},
		{Name: "svc", Dockerfile: "Dockerfile"},
	}}
	b := runJob(t, e, d, BuildRequest{})
	snap := b.snapshot()
	if snap.State != BuildCompleted {
		t.Fatalf("%s: %s", snap.State, snap.ErrorMsg)
	}
	for _, s := range snap.Services {
		if s.Digest != fakeDigest || s.Tag != snap.VersionTag || s.Image != "pre-"+s.Name {
			t.Errorf("service %s result incomplete: %+v", s.Name, s)
		}
	}
	for _, c := range dockerCalls {
		if strings.Contains(c, "imagetools create") && strings.Contains(c, "--tag") && !strings.Contains(c, ":stable") {
			t.Errorf("a versioned tag must never be created by copying :latest (race with a concurrent job): %q", c)
		}
		if strings.Contains(c, "pre-aab") {
			t.Errorf("skip_latest service touched :latest/:stable: %q", c)
		}
	}
}

func TestServiceFailsWhenItsDigestCannotBeRead(t *testing.T) {
	e := newRunEngine(t, Config{FastLane: 1, SlowLane: 1})
	e.execBuildx = func(context.Context, *Build, ServiceSpec, []string, string, func(string)) error { return nil } // writes no metadata
	d := Descriptor{Project: "p", Services: []ServiceSpec{{Name: "a", Dockerfile: "Dockerfile"}}}
	b := runJob(t, e, d, BuildRequest{})
	snap := b.snapshot()
	if snap.State != BuildFailed || !strings.Contains(snap.Services[0].Err, "digest") {
		t.Fatalf("a pushed image with no reportable digest is unusable to consumers (they pull by digest): %+v", snap)
	}
}

func TestReadPushedDigestValidates(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "m.json")
		must(t, os.WriteFile(p, []byte(s), 0o644))
		return p
	}
	if d, err := readPushedDigest(write(`{"containerimage.digest":"` + fakeDigest + `","other":1}`)); err != nil || d != fakeDigest {
		t.Fatalf("%q %v", d, err)
	}
	for _, bad := range []string{`{}`, `{"containerimage.digest":"sha256:short"}`, `not json`, `{"containerimage.digest":"md5:` + strings.Repeat("a", 32) + `"}`} {
		if _, err := readPushedDigest(write(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := readPushedDigest(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("a missing metadata file must error")
	}
}

func TestSnapshotCopiesContextsSoCallersCannotRaceOnThem(t *testing.T) {
	b := newJob(t)
	b.setSources("abc", map[string]ContextResult{"kit": {Revision: "r1"}})
	s := b.snapshot()
	s.Contexts["kit"] = ContextResult{Revision: "mutated"}
	if b.snapshot().Contexts["kit"].Revision != "r1" {
		t.Fatal("snapshot shares the live contexts map")
	}
}

// ---- retention: never delete a digest a kept/protected tag still uses (item f) ------------------

func TestPruneOldTagsKeepsDigestsSharedWithLiveTags(t *testing.T) {
	// 4 timestamp tags, keep 2. The oldest (T1) shares its digest with the newest (T4) — a rebuild
	// that changed nothing — so deleting T1's digest would delete T4. T2 has its own digest.
	digests := map[string]string{
		"20260101-000001": "sha256:aaa", "20260101-000002": "sha256:bbb",
		"20260101-000003": "sha256:ccc", "20260101-000004": "sha256:aaa",
		"latest": "sha256:aaa",
	}
	var deleted []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tags/list"):
			_, _ = w.Write([]byte(`{"tags":["20260101-000001","20260101-000002","20260101-000003","20260101-000004","latest"]}`))
		case r.Method == http.MethodHead:
			tag := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if d, ok := digests[tag]; ok {
				w.Header().Set("Docker-Content-Digest", d)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodDelete:
			deleted = append(deleted, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()
	rc := &registryClient{hc: srv.Client()}
	rc.pruneOldTags(context.Background(), strings.TrimPrefix(srv.URL, "https://"), "img", 2)
	// keep=2 keeps T4 and T3, so T2 (bbb) and T1 (aaa) are stale; aaa is also held by kept T4 and latest.
	for _, d := range deleted {
		if d == "sha256:aaa" {
			t.Fatalf("deleted the digest shared with a kept tag and :latest: %v", deleted)
		}
	}
	if len(deleted) != 1 || deleted[0] != "sha256:bbb" {
		t.Fatalf("expected only the unshared stale digest bbb to go, got %v", deleted)
	}
}

// ---- handler: over-ceiling manifest is a 400 with a clear message -------------------------------

func TestTriggerRejectsAMemoryAskAboveTheCeiling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := &app{cfg: Config{MemoryCeiling: 7 << 30}}
	body := `{"descriptor":{"project":"p","services":[{"name":"android","memory":"16g"}]},"registry":"r","repo":"x"}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/builds", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	a.handleTriggerBuild(c)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "BUILD_MEMORY_CEILING") || !strings.Contains(w.Body.String(), "android") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}
