package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProvenanceForGitPath(t *testing.T) {
	p := provenanceFor(BuildRequest{Repo: "https://github.com/blakeblackshear/frigate.git", Ref: "0.19"})
	if p.Context != contextGit {
		t.Errorf("clone path => context %q; got %q", contextGit, p.Context)
	}
	if p.Repo != "https://github.com/blakeblackshear/frigate.git" || p.Ref != "0.19" {
		t.Errorf("repo/ref carried through; got %+v", p)
	}
	if p.Revision != "" {
		t.Errorf("revision is not knowable before the clone; got %q", p.Revision)
	}
}

// The uploaded-context path must claim NOTHING about a repo, even when the
// request carries those fields — the tar did not come from them. Without this
// the labels would name a source the image was not built from, which is worse
// than no label at all.
func TestProvenanceForUploadPathIgnoresRepoAndRef(t *testing.T) {
	p := provenanceFor(BuildRequest{
		ContextB64: "H4sIAAAAAAAA",
		Repo:       "https://github.com/some/other.git",
		Ref:        "main",
	})
	if p.Context != contextUpload {
		t.Errorf("upload path => context %q; got %q", contextUpload, p.Context)
	}
	if p.Repo != "" || p.Ref != "" || p.Revision != "" {
		t.Errorf("upload path must claim no source; got %+v", p)
	}
	// It still has to be identifiable as ours — that is this label's whole job.
	if got := strings.Join(p.labelArgs(), " "); got != "--label "+labelContext+"=upload" {
		t.Errorf("upload build must carry exactly the context label; got %q", got)
	}
}

func TestLabelArgsOmitsUnknownFieldsEntirely(t *testing.T) {
	if got := (provenance{}).labelArgs(); len(got) != 0 {
		t.Errorf("zero provenance => no flags; got %v", got)
	}
	// A partially-known provenance must not emit "key=" for the unknown parts:
	// an empty source_ref downstream reads as "a branch named empty string".
	got := strings.Join(provenance{Context: contextGit, Repo: "https://example.invalid/r.git"}.labelArgs(), " ")
	for _, absent := range []string{labelRevision, labelRef} {
		if strings.Contains(got, absent) {
			t.Errorf("unknown %s must be omitted, not empty; got %q", absent, got)
		}
	}
	if strings.Contains(got, "=\"\"") || strings.HasSuffix(got, "=") || strings.Contains(got, "= ") {
		t.Errorf("no label may be emitted with an empty value; got %q", got)
	}
}

func TestLabelArgsOrderIsDeterministic(t *testing.T) {
	p := provenance{Context: contextGit, Repo: "r", Ref: "f", Revision: "abc"}
	want := []string{
		"--label", labelSource + "=r",
		"--label", labelRevision + "=abc",
		"--label", labelRef + "=f",
		"--label", labelContext + "=git",
	}
	got := p.labelArgs()
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv must be deterministic\n want %v\n  got %v", want, got)
	}
}

func TestSanitizeLabelValue(t *testing.T) {
	cases := []struct{ in, want, why string }{
		{"0.19", "0.19", "ordinary ref"},
		{"  0.19  ", "0.19", "trimmed"},
		{"", "", "empty"},
		{"   ", "", "whitespace only"},
		{"0.19\nLABEL evil=1", "", "newline dropped, not repaired"},
		{"0.19\rx", "", "carriage return dropped"},
		{"0.19\tx", "", "tab dropped"},
		{"a\x7fb", "", "DEL dropped"},
		{strings.Repeat("a", maxLabelValue), strings.Repeat("a", maxLabelValue), "at the cap"},
		{strings.Repeat("a", maxLabelValue+1), "", "over the cap is dropped, never truncated"},
		{"feature/über-branch", "feature/über-branch", "non-ascii is fine"},
	}
	for _, c := range cases {
		if got := sanitizeLabelValue(c.in); got != c.want {
			t.Errorf("%s: sanitize(%q) = %q, want %q", c.why, c.in, got, c.want)
		}
	}
}

func TestScrubURLCredentials(t *testing.T) {
	cases := []struct{ in, want, why string }{
		{
			"https://x-access-token:ghs_SECRETTOKEN@github.com/Daniel-dev22/agent-kit-go.git",
			"https://github.com/Daniel-dev22/agent-kit-go.git",
			"a vended token must never reach an image label",
		},
		{"https://user@github.com/o/r.git", "https://github.com/o/r.git", "bare userinfo also stripped"},
		{"https://github.com/o/r.git", "https://github.com/o/r.git", "no userinfo, unchanged"},
		{"git@github.com:o/r.git", "git@github.com:o/r.git", "scp syntax keeps its ssh user"},
		{"  https://github.com/o/r.git  ", "https://github.com/o/r.git", "trimmed"},
		{"https://exa mple.com/\x7f", "", "unparseable with a scheme is omitted, not guessed"},
	}
	for _, c := range cases {
		if got := scrubURLCredentials(c.in); got != c.want {
			t.Errorf("%s: scrub(%q) = %q, want %q", c.why, c.in, got, c.want)
		}
	}
	// The property, stated directly: no output may contain a secret from the input.
	if got := scrubURLCredentials("https://x-access-token:ghs_SECRETTOKEN@github.com/o/r.git"); strings.Contains(got, "ghs_SECRETTOKEN") {
		t.Fatalf("token survived scrubbing: %q", got)
	}
}

func TestBuildxArgsCarriesLabelsBeforeTheTail(t *testing.T) {
	d := Descriptor{Platform: PlatformSpec{Mode: "fixed", Value: "linux/amd64"}}
	svc := ServiceSpec{Name: "app", Context: ".", Dockerfile: "Dockerfile"}
	prov := provenance{Context: contextGit, Repo: "https://github.com/o/r.git", Ref: "0.19", Revision: "deadbeef"}
	args := buildxArgs("b", d, svc, "reg/app", "Dockerfile", "v", "a", "", prov)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		labelSource + "=https://github.com/o/r.git",
		labelRevision + "=deadbeef",
		labelRef + "=0.19",
		labelContext + "=git",
	} {
		if !strings.Contains(joined, "--label "+want) {
			t.Errorf("missing --label %s in %q", want, joined)
		}
	}
	// buildx requires flags before the trailing context argument.
	if li, pi := strings.LastIndex(joined, "--label"), strings.Index(joined, "--push"); li < 0 || pi < 0 || li > pi {
		t.Errorf("labels must precede the -t/-f/--push tail; got %q", joined)
	}
	// A build with no provenance must produce the byte-identical argv it did
	// before this change — the phase is additive or it is a regression.
	if strings.Contains(strings.Join(buildxArgs("b", d, svc, "reg/app", "Dockerfile", "v", "a", "", provenance{}), " "), "--label") {
		t.Error("zero provenance must add no flags at all")
	}
}

// gitRevision drives the real git binary against a real repository: the thing it
// parses is git's output, so a fake would only prove we can read our own string.
func TestGitRevisionReadsTheClonedCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed; gitRevision has nothing to drive")
	}
	dir := t.TempDir()
	// Identity comes from the environment rather than `git -c user.email`, so the
	// fixture is hermetic (no dependency on the host's global config) and no real
	// address is written anywhere. RFC 2606 name per no-infra-fingerprints.
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"commit", "-q", "--allow-empty", "-m", "x"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	want, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("reference rev-parse: %v", err)
	}
	e := &engine{cfg: Config{GitTimeout: 30 * time.Second}}
	got := e.gitRevision(context.Background(), &Build{}, dir)
	if got != strings.TrimSpace(string(want)) {
		t.Errorf("gitRevision = %q, want %q", got, strings.TrimSpace(string(want)))
	}
	if strings.ContainsAny(got, " \n\r\t") {
		t.Errorf("revision must be bare, got %q", got)
	}
}

// A revision we cannot read is a missing label, never a failed build: metadata
// must not be able to veto the thing it describes.
func TestGitRevisionFailureIsEmptyAndLogged(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed; gitRevision has nothing to drive")
	}
	e := &engine{cfg: Config{GitTimeout: 30 * time.Second}}

	// The two ways this actually fails produce different messages from git, and
	// the point of capturing stderr is that the log distinguishes them. This
	// failure is silent by design — the build still succeeds — so the log line is
	// the entire diagnosis anyone will ever get; "exit status 128" is not one.
	existing := t.TempDir()
	for _, c := range []struct{ dir, want string }{
		{filepath.Join(t.TempDir(), "absent"), "No such file or directory"},
		{existing, "not a git repository"},
	} {
		b := &Build{}
		if got := e.gitRevision(context.Background(), b, c.dir); got != "" {
			t.Errorf("%s: non-repo => empty revision; got %q", c.want, got)
		}
		if b.LineCount == 0 {
			t.Fatalf("%s: a dropped label must leave a trace in the build log", c.want)
		}
		line := b.ringBuffer[len(b.ringBuffer)-1]
		if !strings.Contains(line, c.want) {
			t.Errorf("log line must carry git's own reason (%q), got %q", c.want, line)
		}
		if strings.Contains(line, "exit status") && !strings.Contains(line, c.want) {
			t.Errorf("log line degraded to the bare exit code: %q", line)
		}
	}
	if got := (provenance{Context: contextGit, Repo: "r", Ref: "f"}).labelArgs(); strings.Contains(strings.Join(got, " "), labelRevision) {
		t.Errorf("an unread revision must omit its label; got %v", got)
	}
}

// The clip only fires on a stderr longer than the cap, which no ordinary failure
// produces — so without this the cap could be moved, or removed, with the suite
// green. git names the offending path in its message, so a long enough path
// drives the real truncation through the real function rather than a helper.
func TestClipStderrBoundary(t *testing.T) {
	// The cap is a deliberate policy number, asserted as a literal so widening it
	// is a decision someone makes on purpose rather than a silent drift.
	if maxLoggedStderr != 200 {
		t.Fatalf("log clip is %d; changing it is fine, but do it deliberately", maxLoggedStderr)
	}
	for _, n := range []int{0, 1, maxLoggedStderr - 1, maxLoggedStderr} {
		in := strings.Repeat("x", n)
		if got := clipStderr(in); got != in {
			t.Errorf("%d bytes is at or under the cap and must pass through whole; got %d bytes", n, len(got))
		}
	}
	// One byte over is the only input that distinguishes > from >=.
	over := strings.Repeat("x", maxLoggedStderr+1)
	got := clipStderr(over)
	if got != strings.Repeat("x", maxLoggedStderr)+"…" {
		t.Errorf("one byte over must clip to exactly the cap plus a marker; got %d bytes, suffix %q", len(got), got[len(got)-4:])
	}
	if clipStderr("  padded  ") != "padded" {
		t.Error("clipStderr trims, so an all-whitespace stderr falls back to the exit error")
	}
}

func TestGitRevisionClipsARunawayStderr(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed; gitRevision has nothing to drive")
	}
	// Never created: git fails to chdir and echoes the whole path back.
	long := filepath.Join(t.TempDir(), strings.Repeat("a", 200), strings.Repeat("b", 200))
	e := &engine{cfg: Config{GitTimeout: 30 * time.Second}}
	b := &Build{}
	if got := e.gitRevision(context.Background(), b, long); got != "" {
		t.Errorf("non-repo => empty revision; got %q", got)
	}
	if b.LineCount == 0 {
		t.Fatal("expected a log line")
	}
	line := b.ringBuffer[len(b.ringBuffer)-1]
	if !strings.HasSuffix(line, "…") {
		t.Errorf("an over-cap stderr must be clipped and marked; got %q", line)
	}
	// The cap bounds the git message, not the whole line — the prefix is ours.
	const prefix = "[git] rev-parse HEAD failed, omitting the revision label: "
	if got := len([]byte(strings.TrimSuffix(strings.TrimPrefix(line, prefix), "…"))); got != maxLoggedStderr {
		t.Errorf("clipped message = %d bytes, want exactly %d", got, maxLoggedStderr)
	}
}

// engine.run needs docker, git, a builder and a registry, so no test drives it.
// That makes the wiring itself the untested seam — and the whole phase is inert
// if run computes a provenance and hands buildService a different one. These
// assertions read run's own AST rather than the file, so a matching string in a
// comment or in another function cannot satisfy them.
func TestRunWiresProvenanceIntoBuildService(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "engine.go", nil, 0)
	if err != nil {
		t.Fatalf("parse engine.go: %v", err)
	}
	var run *ast.FuncDecl
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if ok && fd.Name.Name == "run" && fd.Recv != nil {
			run = fd
			break
		}
	}
	if run == nil {
		t.Fatal("engine.run not found — this guard is scoped to its body and cannot run")
	}

	var callsProvenanceFor, setsRevision, passesProvIdent bool
	var buildServiceCalls int
	ast.Inspect(run.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CallExpr:
			switch fn := v.Fun.(type) {
			case *ast.Ident:
				if fn.Name == "provenanceFor" {
					callsProvenanceFor = true
				}
			case *ast.SelectorExpr:
				if fn.Sel.Name == "buildService" {
					buildServiceCalls++
					for _, a := range v.Args {
						// Must be the variable, not a fresh provenance{} literal:
						// a composite literal here keeps every unit test green and
						// ships images with no labels.
						if id, ok := a.(*ast.Ident); ok && id.Name == "prov" {
							passesProvIdent = true
						}
					}
				}
			}
		case *ast.AssignStmt:
			for _, lhs := range v.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Revision" {
					continue
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "prov" {
					setsRevision = true
				}
			}
		}
		return true
	})

	if !callsProvenanceFor {
		t.Error("run must derive its provenance from the request (provenanceFor)")
	}
	if !setsRevision {
		t.Error("run must assign prov.Revision after the clone, or every image ships without one")
	}
	if buildServiceCalls != 1 {
		t.Fatalf("expected exactly 1 buildService call site in run, found %d — this guard pins them all", buildServiceCalls)
	}
	if !passesProvIdent {
		t.Error("run must pass the derived `prov` to buildService, not a fresh literal")
	}
}

// The four keys are a contract with docker-agent, which reads them back off the
// image (docker-agent/provenance.go). Pinned as literals, not derived from the
// constants, so a typo here fails HERE rather than silently producing images whose
// provenance no consumer recognises — a failure that looks exactly like "nothing
// has been rebuilt yet". docker-agent has the mirror of this test.
//
// The keys are duplicated rather than hoisted into agent-kit-go on purpose: the kit
// is a public module and com.controlcenter.* is this estate's namespace. See D7 in
// control-center/docs/BUILD_PROVENANCE_PLAN.md.
func TestLabelKeysAreTheContractDockerAgentReads(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{labelSource, "org.opencontainers.image.source"},
		{labelRevision, "org.opencontainers.image.revision"},
		{labelRef, "com.controlcenter.build.ref"},
		{labelContext, "com.controlcenter.build.context"},
		{contextGit, "git"},
		{contextUpload, "upload"},
	} {
		if c.got != c.want {
			t.Errorf("label contract drifted: this repo writes %q, docker-agent reads %q", c.got, c.want)
		}
	}
}
