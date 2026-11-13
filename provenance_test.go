package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestProvenanceForGitPath(t *testing.T) {
	p := provenanceFor(BuildRequest{Repo: "https://github.com/blakeblackshear/frigate.git", Ref: "0.19"})
	if p.Context != contextGit {
		t.Errorf("clone path => context %q; got %q", contextGit, p.Context)
	}
	// Normalized, not verbatim: the .git suffix is dropped so the label parses as
	// an owner/repo slug downstream. See TestNormalizeRepoURL.
	if p.Repo != "https://github.com/blakeblackshear/frigate" || p.Ref != "0.19" {
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

// Every labelArgs fixture used pre-clean values, so the sanitiser could be
// bypassed entirely in the shipped path with the suite green.
func TestLabelArgsSanitizesThroughTheShippedPath(t *testing.T) {
	for _, c := range []struct {
		name string
		p    provenance
	}{
		{"newline in ref", provenance{Context: contextGit, Ref: "0.19\nLABEL evil=1"}},
		{"over-length ref", provenance{Context: contextGit, Ref: strings.Repeat("a", maxLabelValue+1)}},
		{"C1 control in ref", provenance{Context: contextGit, Ref: "0.19\u0085x"}},
		{"bidi override in ref", provenance{Context: contextGit, Ref: "0.19\u202ex"}},
		{"line separator in repo", provenance{Context: contextGit, Repo: "https://example.invalid/\u2028x"}},
	} {
		got := strings.Join(c.p.labelArgs(), " ")
		if strings.Contains(got, labelRef) && c.p.Ref != "" {
			t.Errorf("%s: a value the sanitiser rejects still reached the argv: %q", c.name, got)
		}
		if strings.Contains(got, labelSource) && c.p.Repo != "" {
			t.Errorf("%s: rejected repo still reached the argv: %q", c.name, got)
		}
		// The always-set context label must survive regardless.
		if !strings.Contains(got, labelContext+"=git") {
			t.Errorf("%s: context label must still be emitted, got %q", c.name, got)
		}
	}
}

// The cap is a policy number. Binding it to itself (the fixtures build their
// inputs from the constant) meant widening it widened the test with it.
func TestLabelValueCapIsAPolicyNumber(t *testing.T) {
	if maxLabelValue != 512 {
		t.Fatalf("label cap is %d; changing it is fine, but do it deliberately", maxLabelValue)
	}
	if got := sanitizeLabelValue(strings.Repeat("a", 513)); got != "" {
		t.Errorf("513 bytes must be dropped, got %d bytes", len(got))
	}
	if got := sanitizeLabelValue(strings.Repeat("a", 512)); len(got) != 512 {
		t.Errorf("512 bytes must pass, got %d", len(got))
	}
}

func TestAllowedLabelRuneRejectsSpoofingAndSeparators(t *testing.T) {
	for _, r := range []rune{0x00, 0x1f, 0x7f, 0x85, 0x9f, 0x2028, 0x2029, 0x202e, 0x2066, 0xfeff} {
		if allowedLabelRune(r) {
			t.Errorf("U+%04X must be rejected", r)
		}
	}
	for _, r := range []rune{'a', '0', '/', '.', '-', '_', 'ü', '中'} {
		if !allowedLabelRune(r) {
			t.Errorf("%q must be allowed", r)
		}
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

func TestNormalizeRepoURL(t *testing.T) {
	const secret = "ghs_SECRETTOKEN"
	cases := []struct{ in, want, why string }{
		{"https://x-access-token:" + secret + "@github.com/o/r.git", "https://github.com/o/r", "vended token stripped"},
		{"git::https://x-access-token:" + secret + "@github.com/o/r.git", "", "transport helper is not reducible; dropped"},
		{"ext::https://x-access-token:" + secret + "@github.com/o/r.git", "", "remote helper dropped"},
		{"https://github.com/o/r.git?access_token=" + secret, "https://github.com/o/r", "credential in the query"},
		{"https://github.com/o/r.git#" + secret, "https://github.com/o/r", "credential in the fragment"},
		{"git@github.com:o/r.git", "https://github.com/o/r", "scp form becomes an https URL"},
		{"git@github.com:Daniel-dev22/build-agent.git", "https://github.com/Daniel-dev22/build-agent", "the estate's actual configured form"},
		{"https://github.com/o/r", "https://github.com/o/r", "already conformant"},
		{"  https://github.com/o/r.git  ", "https://github.com/o/r", "trimmed"},
		{"", "", "empty"},
		{"not a url", "", "unrecognised shape is dropped, not guessed"},
		{"https://exa mple.com/\x7f", "", "unparseable"},
		{"https://github.com/o/r::x", "", "a path bearing :: is not reducible; the guard is load-bearing here"},
	}
	for _, c := range cases {
		got := normalizeRepoURL(c.in)
		if got != c.want {
			t.Errorf("%s: normalizeRepoURL(%q) = %q, want %q", c.why, c.in, got, c.want)
		}
		if strings.Contains(got, secret) {
			t.Fatalf("%s: credential survived: %q", c.why, got)
		}
	}
}

// The value has a CONSUMER: docker-agent's sourceRepoFromLabels parses "owner/repo"
// out of this label to pick an update strategy. Emitting the estate's configured
// scp form fed it a slug that cannot exist. This asserts the shape that parser
// needs, so the coupling is pinned on the writing side too.
func TestNormalizedRepoURLIsParseableAsAnOwnerRepoSlug(t *testing.T) {
	for _, in := range []string{
		"git@github.com:Daniel-dev22/build-agent.git",
		"https://github.com/blakeblackshear/frigate.git",
	} {
		got := normalizeRepoURL(in)
		slug := strings.TrimPrefix(got, "https://github.com/")
		if strings.Count(slug, "/") != 1 || strings.HasSuffix(slug, ".git") || strings.ContainsAny(slug, "@:") {
			t.Errorf("%q -> %q: slug %q is not a clean owner/repo", in, got, slug)
		}
	}
}

// The scrub is pinned at the CALL SITE, not only in the helper: review showed a
// mutation removing the call from provenanceFor survived, and that is the one
// whose cost is unretractable once an image is pushed.
func TestProvenanceForNormalizesTheRepoItStamps(t *testing.T) {
	const secret = "ghs_SECRETTOKEN"
	p := provenanceFor(BuildRequest{Repo: "https://x-access-token:" + secret + "@github.com/o/r.git", Ref: "main"})
	if strings.Contains(p.Repo, secret) {
		t.Fatalf("provenanceFor stamped a credential: %q", p.Repo)
	}
	if p.Repo != "https://github.com/o/r" {
		t.Errorf("provenanceFor must normalize, got %q", p.Repo)
	}
	if got := strings.Join(p.labelArgs(), " "); strings.Contains(got, secret) {
		t.Fatalf("credential reached the argv: %q", got)
	}
	// scp form is what ansible actually configures for every first-party repo.
	if got := provenanceFor(BuildRequest{Repo: "git@github.com:Daniel-dev22/build-agent.git"}).Repo; got != "https://github.com/Daniel-dev22/build-agent" {
		t.Errorf("scp repo not normalized, got %q", got)
	}
}

// "context is ALWAYS set" had an untested third shape: a git request with no repo.
func TestProvenanceForAlwaysSetsAContext(t *testing.T) {
	for _, req := range []BuildRequest{
		{},
		{Repo: "https://github.com/o/r"},
		{Repo: "https://github.com/o/r", Ref: "0.19"},
	} {
		if got := provenanceFor(req).Context; got != contextGit {
			t.Errorf("non-upload request must be context=git, got %q for %+v", got, req)
		}
	}
	if got := provenanceFor(BuildRequest{ContextB64: "x"}).Context; got != contextUpload {
		t.Errorf("upload request must be context=upload, got %q", got)
	}
}

func TestBuildxArgsCarriesLabelsBeforeTheTail(t *testing.T) {
	d := Descriptor{Platform: PlatformSpec{Mode: "fixed", Value: "linux/amd64"}}
	svc := ServiceSpec{Name: "app", Context: ".", Dockerfile: "Dockerfile"}
	prov := provenance{Context: contextGit, Repo: "https://github.com/o/r.git", Ref: "0.19", Revision: "deadbeef"}
	args := buildxArgs("b", d, svc, "reg/app", "Dockerfile", "v", "a", "", prov, buildExtras{})
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
	// A build with no provenance must produce the argv it did before provenance existed (the versioned tag is now pushed by the build itself).
	// Compared against a golden slice, not "does not contain --label": the latter
	// passes for any OTHER flag this function might grow by accident.
	want := []string{
		"buildx", "build", "--builder", "b", "--progress=plain",
		"--platform", "linux/amd64",
		"--cache-from", "type=registry,ref=reg/app:buildcache",
		"--cache-to", "type=registry,ref=reg/app:buildcache,mode=max",
		"-t", "reg/app:latest", "-t", "reg/app:v", "-f", "Dockerfile", "--push", ".",
	}
	got := buildxArgs("b", d, svc, "reg/app", "Dockerfile", "v", "a", "", provenance{}, buildExtras{})
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("zero provenance must reproduce the pre-phase argv exactly\n want %v\n  got %v", want, got)
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

	// Review defeated the four assertions above with a single added line
	// `prov = provenance{}` between the clone and the build loop: every rule still
	// held and every image shipped label-free. Shape is not dataflow — so pin that
	// `prov` is assigned WHOLESALE exactly once (its `:=`), and that only its
	// .Revision field is written afterwards.
	wholesale, fieldWrites := 0, 0
	revisionFromGit := false
	ast.Inspect(run.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range as.Lhs {
			switch v := lhs.(type) {
			case *ast.Ident:
				if v.Name == "prov" {
					wholesale++
				}
			case *ast.SelectorExpr:
				if id, ok := v.X.(*ast.Ident); ok && id.Name == "prov" {
					fieldWrites++
					// The RHS matters as much as the LHS: `prov.Revision = ""`
					// satisfies a check that only inspects what is written to.
					if len(as.Rhs) == 1 {
						if call, ok := as.Rhs[0].(*ast.CallExpr); ok {
							if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "gitRevision" {
								revisionFromGit = true
							}
						}
					}
				}
			}
		}
		return true
	})
	if !revisionFromGit {
		t.Error("prov.Revision must be assigned from e.gitRevision(...), not a literal")
	}
	if wholesale != 1 {
		t.Errorf("prov must be assigned wholesale exactly once (its :=), found %d — a later reassignment silently empties every label", wholesale)
	}
	if fieldWrites != 1 {
		t.Errorf("exactly one field write to prov expected (.Revision), found %d", fieldWrites)
	}
}

// The hop no AST rule scoped to run() can see, and the one review showed nothing
// pinned: buildService handing buildxArgs the provenance it was given. Driven
// through the real buildService via the execBuildx seam, so the assertion is on
// the argv that would actually reach docker.
func TestBuildServicePassesItsProvenanceIntoTheArgv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var captured []string
	e := &engine{
		cfg: Config{Builder: "b"},
		reg: newRegistryClient(),
		execBuildx: func(_ context.Context, _ *Build, _ ServiceSpec, args []string, _ string, _ func(string)) error {
			captured = append([]string(nil), args...)
			return errStopAfterCapture
		},
	}
	b := &Build{dir: dir}
	prov := provenance{Context: contextGit, Repo: "https://github.com/o/r", Ref: "0.19", Revision: "deadbeef"}
	// tagVersioned/pruneOldTags would need docker; the seam returns early so the
	// argv is what we assert on.
	_, err := e.buildService(context.Background(), b, Descriptor{}, ServiceSpec{Name: "app", Dockerfile: "Dockerfile"},
		"reg", "v", "a", "", prov, false, 3, nil, buildExtras{})
	if err == nil {
		t.Fatal("expected the seam's sentinel error")
	}
	joined := strings.Join(captured, " ")
	for _, want := range []string{
		labelSource + "=https://github.com/o/r",
		labelRevision + "=deadbeef",
		labelRef + "=0.19",
		labelContext + "=git",
	} {
		if !strings.Contains(joined, "--label "+want) {
			t.Errorf("buildService dropped provenance on the way to buildxArgs: missing %q in %q", want, joined)
		}
	}
}

var errStopAfterCapture = errors.New("stop after capturing argv")

// gitRevision must not be bounded by the CLONE timeout (5 min by default): it
// reads one file in a clone that already exists, and a wedged git would otherwise
// stall a build for five minutes for a label this code calls best-effort. Neither
// property is observable from a passing call, so both are pinned at the source,
// scoped to this one function's body.
func TestGitRevisionUsesAShortTimeoutAndSuppressesPrompts(t *testing.T) {
	if gitRevParseTimeout != 15*time.Second {
		t.Errorf("rev-parse timeout is %v; change it deliberately", gitRevParseTimeout)
	}
	if gitRevParseTimeout >= 5*time.Minute {
		t.Error("rev-parse must not inherit the clone-scale timeout")
	}
	body := funcBodySource(t, "provenance.go", "gitRevision")
	for _, want := range []string{"gitRevParseTimeout", "GIT_TERMINAL_PROMPT=0", "clipStderr("} {
		if !strings.Contains(body, want) {
			t.Errorf("gitRevision must use %s", want)
		}
	}
	if strings.Contains(body, "e.cfg.GitTimeout") {
		t.Error("gitRevision must not use the clone timeout")
	}
}

// funcBodySource returns the source text of exactly one function's body, so a
// match in a comment elsewhere, or in another function, cannot satisfy a caller.
func funcBodySource(t *testing.T, file, name string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != name || fd.Body == nil {
			continue
		}
		return string(src[fset.Position(fd.Body.Pos()).Offset:fset.Position(fd.Body.End()).Offset])
	}
	t.Fatalf("%s not found in %s — this guard is scoped to it and cannot run", name, file)
	return ""
}

func TestCloneArgsSeparatesOptionsFromPositionals(t *testing.T) {
	withRef := cloneArgs("https://github.com/o/r", "0.19", "/tmp/x")
	want := []string{"clone", "--depth", "1", "--branch", "0.19", "--", "https://github.com/o/r", "/tmp/x"}
	if strings.Join(withRef, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("clone argv\n want %v\n  got %v", want, withRef)
	}
	noRef := cloneArgs("https://github.com/o/r", "", "/tmp/x")
	if strings.Join(noRef, "\x00") != strings.Join([]string{"clone", "--depth", "1", "--", "https://github.com/o/r", "/tmp/x"}, "\x00") {
		t.Errorf("no ref => no --branch, still separated; got %v", noRef)
	}
	// The property, stated directly: every positional follows a bare "--".
	for _, a := range []string{"-oProxyCommand=x", "ext::sh -c id"} {
		got := cloneArgs(a, "", "/tmp/x")
		sep := -1
		for i, v := range got {
			if v == "--" {
				sep = i
				break
			}
		}
		if sep < 0 || got[sep+1] != a {
			t.Errorf("a %q repo must sit after the separator; got %v", a, got)
		}
	}
}

func TestClipStderrCutsOnARuneBoundary(t *testing.T) {
	// 3-byte runes: the cap lands mid-rune, so a byte slice would emit invalid UTF-8.
	in := strings.Repeat("中", maxLoggedStderr)
	got := clipStderr(in)
	if !utf8.ValidString(got) {
		t.Errorf("clipped message is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("over-cap message must be marked, got %q", got)
	}
	if n := len(strings.TrimSuffix(got, "…")); n > maxLoggedStderr {
		t.Errorf("clipped to %d bytes, cap is %d", n, maxLoggedStderr)
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
