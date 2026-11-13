package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- B MED-2: the credential git stores is the base64 header, not only the raw token ------------

func TestEncodedAuthHeaderNeverLeavesTheEnvironment(t *testing.T) {
	fx := newGitFixture(t, "r")
	enc := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + testToken))

	cmd := cloneCmd(context.Background(), "git@github.com:o/r.git", "main", "/tmp/d", testToken)
	for _, a := range cmd.Args {
		// any -c<k=v> / -ck=v / --config...: git clone persists those into .git/config
		if strings.Contains(a, enc) || strings.HasPrefix(a, "-c") || strings.HasPrefix(a, "--config") {
			t.Fatalf("auth must travel in the environment only (git clone -c persists into .git/config): %v", cmd.Args)
		}
	}
	for _, c := range shaCmds(context.Background(), "git@github.com:o/r.git", strings.Repeat("a", 40), "/tmp/d", testToken) {
		for _, a := range c.Args {
			if strings.Contains(a, testToken) || strings.Contains(a, enc) || strings.HasPrefix(a, "-c") || strings.HasPrefix(a, "--config") {
				t.Fatalf("sha fetch leaks credentials in argv: %v", c.Args)
			}
		}
	}

	e := &engine{cfg: Config{GitTimeout: 30 * time.Second}}
	for name, ref := range map[string]string{"branch": "main", "sha": fx.headSHA} {
		b := newJob(t)
		dst := filepath.Join(t.TempDir(), "clone")
		if err := e.gitClone(context.Background(), b, "git@github.com:o/r.git", ref, dst, testToken); err != nil {
			t.Fatalf("%s clone: %v\n%s", name, err, strings.Join(b.logLines(), "\n"))
		}
		_ = filepath.WalkDir(dst, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				data, _ := os.ReadFile(p)
				if strings.Contains(string(data), enc) || strings.Contains(string(data), testToken) {
					t.Errorf("%s: credential (raw or base64 header) in %s", name, p)
				}
			}
			return nil
		})
		if l := strings.Join(b.logLines(), "\n"); strings.Contains(l, enc) || strings.Contains(l, testToken) {
			t.Errorf("%s: credential echoed in build log", name)
		}
	}
}

// ---- B LOW-4: contexts pinned by commit SHA ------------------------------------------------------

func TestContextPinnedByShaIsFetchedAndVerified(t *testing.T) {
	fx := newGitFixture(t, "kit")
	e := &engine{cfg: Config{GitTimeout: 30 * time.Second}}
	d := Descriptor{Contexts: map[string]ContextSpec{"kit": {Repo: fx.repoURLFor("kit"), Ref: fx.tagSHA}}}
	_, res, err := e.cloneContexts(context.Background(), newJob(t), d, BuildRequest{}, "")
	if err != nil || res["kit"].Revision != fx.tagSHA {
		t.Fatalf("SHA pin (older commit, not a branch tip): %v %+v want %s", err, res, fx.tagSHA)
	}
	d2 := Descriptor{Contexts: map[string]ContextSpec{"kit": {Repo: fx.repoURLFor("kit"), Ref: strings.Repeat("0", 40)}}}
	if _, _, err := e.cloneContexts(context.Background(), newJob(t), d2, BuildRequest{}, ""); err == nil {
		t.Fatal("a SHA that does not exist must fail the job")
	}
}

// ---- B LOW-2: repo restriction ------------------------------------------------------------------

func TestContextRepoMustBeGithub(t *testing.T) {
	for _, bad := range []string{"file:///var/lib/build-agent/work/other/src", "/var/lib/x", "../x", "ext::sh -c id", "https://evil.example/o/r.git", "ssh://git@github.com/o/r"} {
		if err := validateContexts(map[string]ContextSpec{"k": {Repo: bad}}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	for _, ok := range []string{"https://github.com/o/r.git", "git@github.com:o/r.git"} {
		if err := validateContexts(map[string]ContextSpec{"k": {Repo: ok}}); err != nil {
			t.Errorf("rejected %q: %v", ok, err)
		}
	}
}

// ---- B LOW-1: a private context must not hand the token to an uploaded Dockerfile ----------------

func TestPrivateContextDoesNotGiveAnUploadedBuildTheTokenSecret(t *testing.T) {
	fx := newGitFixture(t, "kit")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"token":"` + testToken + `","expires_at":"2099-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	cfg := Config{FastLane: 1, SlowLane: 1, PrivateRepoOwners: []string{"o"}, ControlCenterURL: srv.URL}
	e := newRunEngine(t, cfg)
	e.cc = srv.Client()
	var argv []string
	e.execBuildx = fakeBuildx(func(_ ServiceSpec, a []string) { argv = append([]string(nil), a...) })
	d := Descriptor{Project: "p", Services: []ServiceSpec{{Name: "a", Dockerfile: "Dockerfile"}},
		Contexts: map[string]ContextSpec{"kit": {Repo: fx.repoURLFor("kit"), Ref: "v1"}}}
	b := runJob(t, e, d, BuildRequest{}) // uploaded app context
	if st := b.snapshot(); st.State != BuildCompleted || st.Contexts["kit"].Revision != fx.tagSHA {
		t.Fatalf("%s: %s\n%s", st.State, st.ErrorMsg, strings.Join(b.logLines(), "\n"))
	}
	if strings.Contains(strings.Join(argv, " "), "--secret") {
		t.Fatalf("the installation token reached an uploaded-tarball Dockerfile: %v", argv)
	}
}

// ---- A MED-2: retention fails closed -------------------------------------------------------------

func TestRetentionSkipsPruneWhenALiveDigestCannotBeResolved(t *testing.T) {
	digests := map[string]string{"20260101-000001": "sha256:aaa", "20260101-000002": "sha256:bbb",
		"20260101-000003": "sha256:ccc", "20260101-000004": "sha256:aaa"} // T1 shares aaa with the kept T4
	failLatest := true
	var deleted []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tag := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"tags":["20260101-000001","20260101-000002","20260101-000003","20260101-000004","latest","1.2.3"]}`))
		case r.Method == http.MethodHead:
			if tag == "latest" && failLatest {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			d := digests[tag]
			if tag == "latest" || tag == "1.2.3" {
				d = "sha256:aaa"
			}
			w.Header().Set("Docker-Content-Digest", d)
		case r.Method == http.MethodDelete:
			deleted = append(deleted, tag)
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()
	rc := &registryClient{hc: srv.Client()}
	rc.pruneOldTags(context.Background(), strings.TrimPrefix(srv.URL, "https://"), "img", 2)
	if len(deleted) != 0 {
		t.Fatalf("a live digest was unresolvable: nothing may be deleted, got %v", deleted)
	}
	failLatest = false
	rc.pruneOldTags(context.Background(), strings.TrimPrefix(srv.URL, "https://"), "img", 2)
	if len(deleted) != 1 || deleted[0] != "sha256:bbb" {
		t.Fatalf("with everything resolvable only the unshared stale digest goes (semver tag 1.2.3 also holds aaa): %v", deleted)
	}
}

// ---- A MED-3: queueing behind another job must not spend BUILD_TIMEOUT ---------------------------

func TestAdmissionWaitDoesNotConsumeTheBuildTimeout(t *testing.T) {
	e := newRunEngine(t, Config{FastLane: 1, SlowLane: 1, BuildTimeout: 400 * time.Millisecond})
	started := make(chan struct{})
	release := make(chan struct{})
	e.execBuildx = fakeBuildx(func(svc ServiceSpec, _ []string) {
		if svc.Name == "first" {
			close(started)
			<-release
		}
	})
	d1 := Descriptor{Project: "a", Services: []ServiceSpec{{Name: "first", Dockerfile: "Dockerfile", Lane: "slow"}}}
	d2 := Descriptor{Project: "b", Services: []ServiceSpec{{Name: "second", Dockerfile: "Dockerfile", Lane: "slow"}}}
	done1 := make(chan *Build)
	go func() { done1 <- runJob(t, e, d1, BuildRequest{}) }()
	<-started
	done2 := make(chan *Build)
	go func() { done2 <- runJob(t, e, d2, BuildRequest{}) }()
	time.Sleep(700 * time.Millisecond) // second waits LONGER than BuildTimeout for the slow lane
	close(release)
	<-done1
	b2 := <-done2
	if st := b2.snapshot(); st.State != BuildCompleted {
		t.Fatalf("a job that merely queued past BUILD_TIMEOUT must still build: %s %s", st.State, st.ErrorMsg)
	}
}

func TestBuildTimeoutStillBoundsAnAdmittedBuild(t *testing.T) {
	e := newRunEngine(t, Config{FastLane: 1, SlowLane: 1, BuildTimeout: 100 * time.Millisecond})
	e.execBuildx = func(ctx context.Context, _ *Build, _ ServiceSpec, _ []string, _ string, _ func(string)) error {
		<-ctx.Done()
		return ctx.Err()
	}
	b := runJob(t, e, Descriptor{Project: "p", Services: []ServiceSpec{{Name: "a", Dockerfile: "Dockerfile"}}}, BuildRequest{})
	if st := b.snapshot(); st.State != BuildFailed {
		t.Fatalf("a hung build must be cut by BUILD_TIMEOUT: %s", st.State)
	}
}

var _ = exec.Command

// A SHA that names an annotated tag OBJECT peels to a different commit on checkout; the pin must
// then fail loudly rather than build a commit nobody pinned.
func TestShaPinOfAnAnnotatedTagObjectFailsTheMismatchCheck(t *testing.T) {
	fx := newGitFixture(t, "kit")
	if fx.annotSHA == "" || fx.annotSHA == fx.tagSHA {
		t.Fatal("fixture must produce a tag object distinct from its commit")
	}
	e := &engine{cfg: Config{GitTimeout: 30 * time.Second}}
	d := Descriptor{Contexts: map[string]ContextSpec{"kit": {Repo: fx.repoURLFor("kit"), Ref: fx.annotSHA}}}
	_, _, err := e.cloneContexts(context.Background(), newJob(t), d, BuildRequest{}, "")
	if err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("want the pinned-but-checked-out mismatch error, got %v", err)
	}
}

// ---- LOW-1: retention escalation ------------------------------------------------------------------

func retentionServer(t *testing.T, headStatus func(tag string) int) (*registryClient, string, *[]string) {
	t.Helper()
	var deleted []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tag := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte("{\"tags\":[\"20260101-000001\",\"20260101-000002\",\"20260101-000003\",\"20260101-000004\",\"gone\"]}"))
		case http.MethodHead:
			if st := headStatus(tag); st != http.StatusOK {
				w.WriteHeader(st)
				return
			}
			w.Header().Set("Docker-Content-Digest", "sha256:"+tag)
		case http.MethodDelete:
			deleted = append(deleted, tag)
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(srv.Close)
	return &registryClient{hc: srv.Client()}, strings.TrimPrefix(srv.URL, "https://"), &deleted
}

func TestRetentionIgnoresADanglingLiveTagButStopsOnA5xx(t *testing.T) {
	status := http.StatusNotFound
	rc, reg, deleted := retentionServer(t, func(tag string) int {
		if tag == "gone" {
			return status
		}
		return http.StatusOK
	})
	rc.pruneOldTags(context.Background(), reg, "img", 2)
	if len(*deleted) == 0 {
		t.Fatal("a 404 live tag protects nothing: the prune must proceed")
	}
	*deleted = nil
	status = http.StatusInternalServerError
	rc.pruneOldTags(context.Background(), reg, "img", 2)
	if len(*deleted) != 0 {
		t.Fatalf("a 5xx on a live tag must stop the prune: %v", *deleted)
	}
}

func TestRetentionCountsConsecutiveSkipsAndResetsOnSuccess(t *testing.T) {
	status := http.StatusInternalServerError
	rc, reg, _ := retentionServer(t, func(tag string) int {
		if tag == "gone" {
			return status
		}
		return http.StatusOK
	})
	for i := 0; i < 3; i++ {
		rc.pruneOldTags(context.Background(), reg, "img", 2)
	}
	if rc.skips["img"] != 3 {
		t.Fatalf("consecutive skips = %d, want 3 (it is surfaced as an Error from the third)", rc.skips["img"])
	}
	status = http.StatusOK
	rc.pruneOldTags(context.Background(), reg, "img", 2)
	if rc.skips["img"] != 0 {
		t.Fatalf("a completed prune must reset the count, got %d", rc.skips["img"])
	}
}
