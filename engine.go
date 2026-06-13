package main

// engine ports build_orchestrator.py's build logic to Go: clone repo@ref, ensure
// the persistent buildx builder, validate/bust registry caches, build each
// service via `docker buildx` under fast/slow lane semaphores, rotate + version
// tags, prune retention, and bounded-GC. State mutations on the Build flow back
// to the controller + fleet via the onChange callback supplied by the registry.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// buildxStepRe matches buildx `--progress=plain` stage-step lines, e.g.
//
//	#12 [router 4/9] RUN go build ./...
//	#8 [linux/amd64 builder 3/6] COPY . .
//
// The stage label (group 1) may carry a platform prefix; the n/m counter is the
// last token inside the brackets, and the remainder is the step command. Output
// lines (`#12 0.5 …`), `DONE`/`CACHED`, and `[internal]` vertices don't match.
var buildxStepRe = regexp.MustCompile(`^#\d+\s+\[(.+?)\s+(\d+)/(\d+)\]\s*(.*)$`)

// parseBuildxStep extracts the current sub-step from one plain-progress line.
// stepName prefers the command text (e.g. "RUN go build"); it falls back to the
// stage label and is bounded so a giant RUN line can't bloat the snapshot.
func parseBuildxStep(line string) (current, total int, stepName string, ok bool) {
	m := buildxStepRe.FindStringSubmatch(line)
	if m == nil {
		return 0, 0, "", false
	}
	cur, err1 := strconv.Atoi(m[2])
	tot, err2 := strconv.Atoi(m[3])
	if err1 != nil || err2 != nil || tot <= 0 {
		return 0, 0, "", false
	}
	name := strings.TrimSpace(m[4])
	if name == "" {
		name = strings.TrimSpace(m[1])
	}
	if len(name) > 80 {
		name = name[:80]
	}
	return cur, tot, name, true
}

type engine struct {
	cfg Config
	cc  *http.Client
	reg *registryClient
}

func newEngine(cfg Config, cc *http.Client) *engine {
	return &engine{cfg: cfg, cc: cc, reg: newRegistryClient()}
}

func nativePlatform() string {
	switch runtime.GOARCH {
	case "amd64":
		return "linux/amd64"
	case "arm64":
		return "linux/arm64"
	default:
		return "linux/amd64"
	}
}

// platformArgs: a fixed-platform descriptor wins; else multi-arch (auto) fans out
// to amd64+arm64; the agent defaults auto→multi-arch since builds push to a
// multi-arch registry (matches the orchestrator's --multi-arch default in vars).
func platformArgs(d Descriptor) []string {
	if d.Platform.Mode == "fixed" && d.Platform.Value != "" {
		return []string{"--platform", d.Platform.Value}
	}
	return []string{"--platform", "linux/amd64,linux/arm64"}
}

// renderBuildArgs merges project + per-service build_args and substitutes the
// ${VERSION}/${APK_CACHE_BUST}/${SERVICE_DIR} templates.
func renderBuildArgs(d Descriptor, svc ServiceSpec, versionTag, apkBust string) []string {
	merged := map[string]string{}
	for k, v := range d.BuildArgs {
		merged[k] = v
	}
	for k, v := range svc.BuildArgs {
		merged[k] = v
	}
	serviceDir := filepath.Dir(svc.dockerfile())
	if serviceDir == "" {
		serviceDir = "."
	}
	subs := strings.NewReplacer(
		"${VERSION}", versionTag,
		"${APK_CACHE_BUST}", apkBust,
		"${SERVICE_DIR}", serviceDir,
	)
	var out []string
	for k, v := range merged {
		out = append(out, "--build-arg", k+"="+subs.Replace(v))
	}
	return out
}

// gitClone clones repo@ref (shallow) into dir. For private repos a vended GitHub
// App token is injected into the URL; the token never lands on disk in the clone.
func (e *engine) gitClone(ctx context.Context, b *Build, repo, ref, dir, token string) error {
	url := repo
	if token != "" {
		url = githubTokenURL(repo, token)
	}
	cctx, cancel := context.WithTimeout(ctx, e.cfg.GitTimeout)
	defer cancel()
	args := []string{"clone", "--depth", "1"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, url, dir)
	cmd := exec.CommandContext(cctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return e.stream(b, "git", cmd, nil)
}

// ensureBuilder reuses the persistent buildx builder (cache mounts survive); only
// (re)creates it when missing or when CC_RECREATE_BUILDER=1.
func (e *engine) ensureBuilder(ctx context.Context, b *Build) error {
	name := e.cfg.Builder
	if exec.CommandContext(ctx, "docker", "buildx", "inspect", name).Run() == nil &&
		os.Getenv("CC_RECREATE_BUILDER") != "1" {
		b.appendLine("[builder] reusing " + name)
		return nil
	}
	_ = exec.CommandContext(ctx, "docker", "buildx", "rm", name, "--force").Run()
	create := []string{"buildx", "create", "--name", name,
		"--driver", "docker-container", "--driver-opt", "network=host", "--bootstrap"}
	if e.cfg.BuildkitdConfig != "" {
		if _, err := os.Stat(e.cfg.BuildkitdConfig); err == nil {
			create = append(create, "--buildkitd-config", e.cfg.BuildkitdConfig)
		}
	}
	if err := e.stream(b, "builder", exec.CommandContext(ctx, "docker", create...), nil); err != nil {
		return fmt.Errorf("create builder: %w", err)
	}
	b.appendLine("[builder] created " + name)
	return nil
}

func (e *engine) rotateTags(ctx context.Context, full string) {
	if exec.CommandContext(ctx, "docker", "buildx", "imagetools", "inspect", full+":latest").Run() == nil {
		_ = exec.CommandContext(ctx, "docker", "buildx", "imagetools", "create",
			"--tag", full+":stable", full+":latest").Run()
	}
}

func (e *engine) tagVersioned(ctx context.Context, full, tag string) error {
	out, err := exec.CommandContext(ctx, "docker", "buildx", "imagetools", "create",
		"--tag", full+":"+tag, full+":latest").CombinedOutput()
	if err != nil {
		return fmt.Errorf("versioned tag %s: %v: %s", tag, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (e *engine) boundedGC(ctx context.Context) {
	cmd := exec.CommandContext(ctx, "docker", "buildx", "prune", "--builder", e.cfg.Builder, "--force",
		"--reserved-space", "10GB", "--max-used-space", "40GB", "--min-free-space", "20GB",
		"--filter", "until=168h")
	if out, err := cmd.CombinedOutput(); err != nil {
		slog.Warn("bounded gc", "error", err, "out", strings.TrimSpace(string(out)))
	}
}

// buildService runs one service through buildx + tagging + retention. token is
// passed as a buildx secret (id=ghtoken) so the Dockerfile can `go mod download`
// private modules. Returns the resulting image repo path on success.
func (e *engine) buildService(ctx context.Context, b *Build, d Descriptor, svc ServiceSpec, registry, versionTag, apkBust, secretFile string, tagRotation bool, retentionKeep int, notify func()) (string, error) {
	image := d.ImagePrefix + svc.Name
	full := registry + "/" + image
	dockerfile := svc.dockerfile()
	if _, err := os.Stat(filepath.Join(b.workdir(), dockerfile)); err != nil {
		return "", fmt.Errorf("dockerfile %s not found", dockerfile)
	}
	if tagRotation {
		e.rotateTags(ctx, full)
	}
	args := []string{"buildx", "build", "--builder", e.cfg.Builder, "--progress=plain"}
	args = append(args, platformArgs(d)...)
	args = append(args,
		"--cache-from", "type=registry,ref="+full+":buildcache",
		"--cache-to", "type=registry,ref="+full+":buildcache,mode=max")
	args = append(args, renderBuildArgs(d, svc, versionTag, apkBust)...)
	if secretFile != "" {
		args = append(args, "--secret", "id=ghtoken,src="+secretFile)
	}
	args = append(args, "-t", full+":latest", "-f", dockerfile, "--push", svc.buildContext())

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = b.workdir()
	// Parse buildx stage-step lines off the same stream and surface them as a
	// live indicator. Only a real step change broadcasts (setServiceStep dedupes),
	// so the coalescing fleet hub sees at most one trigger per step.
	onLine := func(line string) {
		if cur, tot, name, ok := parseBuildxStep(line); ok {
			if b.setServiceStep(svc.Name, cur, tot, name) && notify != nil {
				notify()
			}
		}
	}
	if err := e.stream(b, svc.Name, cmd, onLine); err != nil {
		return "", err
	}
	if err := e.tagVersioned(ctx, full, versionTag); err != nil {
		return "", err
	}
	e.reg.pruneOldTags(ctx, registry, image, retentionKeep)
	return image, nil
}

// stream runs cmd, tees merged stdout+stderr into the build's log ring buffer
// (prefixed) and returns its exit error. onLine, when non-nil, is also called
// with each raw (unprefixed) line — used by buildService to parse buildx steps.
func (e *engine) stream(b *Build, prefix string, cmd *exec.Cmd, onLine func(string)) error {
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		return err
	}
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			b.appendLine("[" + prefix + "] " + line)
			if onLine != nil {
				onLine(line)
			}
		}
		close(done)
	}()
	werr := cmd.Wait()
	pw.Close()
	<-done
	return werr
}

func (e *engine) laneSem(d Descriptor) (fast, slow chan struct{}) {
	fn := d.Lanes.Fast
	if fn <= 0 {
		fn = e.cfg.FastLane
	}
	sn := d.Lanes.Slow
	if sn <= 0 {
		sn = e.cfg.SlowLane
	}
	return make(chan struct{}, fn), make(chan struct{}, sn)
}

// run orchestrates a whole build invocation, mutating b and notifying via
// onChange on every lifecycle transition. notify is a lightweight fleet-only
// nudge (no controller event) used for live sub-step broadcasts. Blocks until
// terminal.
func (e *engine) run(ctx context.Context, b *Build, req BuildRequest, onChange func(JobEvent), notify func()) {
	d := req.Descriptor
	registry := req.Registry
	versionTag := versionTagFor(d, req)
	apkBust := time.Now().UTC().Format("2006-01") // bucket; orchestrator used ISO year-week
	services := selectServices(d, req.Services)

	b.setRunning(versionTag, registry, services)
	onChange(EventStarted)

	dir := b.workdir()
	defer os.RemoveAll(dir)

	fail := func(msg string) {
		b.markFailed(msg)
		onChange(EventFailed)
	}

	// Context source: an uploaded inline context (genmon-style) extracts into the
	// workdir; otherwise the agent clones repo@ref. Token vend only applies to the
	// clone path — an uploaded context never needs git auth, so secretFile stays
	// empty and buildService skips the buildx secret.
	var secretFile string
	if req.ContextB64 != "" {
		if err := extractContextTarGz(req.ContextB64, dir); err != nil {
			fail("extract context: " + err.Error())
			return
		}
	} else {
		// Private repos vend a short-lived GitHub App installation token from
		// controller; it is used for the clone URL and as a buildx secret for
		// `go mod download` of private modules. Never written into the clone.
		var cloneToken string
		if isPrivate(req) {
			tok, err := vendGitHubToken(ctx, e.cc, e.cfg.ControlCenterURL, []string{repoSlug(req.Repo)})
			if err != nil {
				fail("github token vend: " + err.Error())
				return
			}
			cloneToken = tok.Token
			secretFile = dir + ".ghtoken"
			if err := os.WriteFile(secretFile, []byte(tok.Token), 0o600); err != nil {
				fail("write token secret: " + err.Error())
				return
			}
			defer os.Remove(secretFile)
		}
		if err := e.gitClone(ctx, b, req.Repo, req.Ref, dir, cloneToken); err != nil {
			fail("git clone: " + err.Error())
			return
		}
	}
	if err := e.ensureBuilder(ctx, b); err != nil {
		fail(err.Error())
		return
	}

	// Best-effort registry buildcache validation/bust before building.
	for _, svc := range services {
		image := d.ImagePrefix + svc.Name
		if st, miss := e.reg.validateCache(ctx, registry, image); st == "corrupted" {
			b.appendLine(fmt.Sprintf("[cache] %s corrupted (%d missing blobs) — busting", image, miss))
			e.reg.bustCache(ctx, registry, image)
		}
	}

	tagRotation := d.TagRotation == nil || *d.TagRotation
	retentionKeep := e.cfg.RetentionKeepDefault
	if d.RetentionKeep != nil {
		retentionKeep = *d.RetentionKeep
	}

	fast, slow := e.laneSem(d)
	var wg sync.WaitGroup
	for _, svc := range services {
		wg.Add(1)
		go func(svc ServiceSpec) {
			defer wg.Done()
			sem := fast
			if svc.lane() == "slow" {
				sem = slow
			}
			sem <- struct{}{}
			defer func() { <-sem }()

			b.setServiceState(svc.Name, SvcBuilding, func(sp *ServiceProgress) {
				now := time.Now().UTC()
				sp.StartedAt = &now
				sp.Attempts++
			})
			onChange(EventProgress)

			image, err := e.buildService(ctx, b, d, svc, registry, versionTag, apkBust, secretFile, tagRotation, retentionKeep, notify)
			finish := func(sp *ServiceProgress) {
				now := time.Now().UTC()
				sp.EndedAt = &now
				if sp.StartedAt != nil {
					sp.DurationMs = now.Sub(*sp.StartedAt).Milliseconds()
				}
				// Clear the live sub-step indicator — duration is what a finished
				// service shows; a stale "step 4/9" on a done card would mislead.
				sp.CurrentStep, sp.TotalSteps, sp.StepName = 0, 0, ""
			}
			if err != nil {
				b.setServiceState(svc.Name, SvcFailed, func(sp *ServiceProgress) { finish(sp); sp.Err = err.Error() })
			} else {
				b.setServiceState(svc.Name, SvcDone, func(sp *ServiceProgress) { finish(sp); sp.Image = image; sp.Tag = versionTag })
			}
			onChange(EventProgress)
		}(svc)
	}
	wg.Wait()

	b.finalize()
	snap := b.snapshot()
	if snap.State == BuildCompleted {
		e.boundedGC(ctx)
		onChange(EventCompleted)
	} else if snap.State == BuildCancelled {
		onChange(EventCancelled)
	} else {
		onChange(EventFailed)
	}
}

// versionTagFor returns the external --version for external-scheme descriptors,
// else a YYYYMMDD-HHMMSS timestamp stamped at build start.
func versionTagFor(d Descriptor, req BuildRequest) string {
	if d.Version.Scheme == "external" && req.Version != "" {
		return req.Version
	}
	return time.Now().UTC().Format("20060102-150405")
}

// selectServices returns the descriptor's services filtered to req.Services
// (empty = all), preserving descriptor order.
func selectServices(d Descriptor, want []string) []ServiceSpec {
	if len(want) == 0 {
		return d.Services
	}
	set := map[string]struct{}{}
	for _, w := range want {
		set[w] = struct{}{}
	}
	var out []ServiceSpec
	for _, s := range d.Services {
		if _, ok := set[s.Name]; ok {
			out = append(out, s)
		}
	}
	return out
}

// isPrivate decides whether to vend a token. Explicit req.Private wins; else our
// org's repos are treated as private.
func isPrivate(req BuildRequest) bool {
	if req.Private != nil {
		return *req.Private
	}
	return strings.Contains(req.Repo, "Daniel-dev22/")
}

// repoSlug extracts "owner/repo" from an ssh or https git URL
// (git@github.com:owner/repo.git or https://github.com/owner/repo.git).
func repoSlug(repo string) string {
	s := strings.TrimSuffix(repo, ".git")
	if i := strings.Index(s, "github.com:"); i >= 0 {
		return s[i+len("github.com:"):]
	}
	if i := strings.Index(s, "github.com/"); i >= 0 {
		return s[i+len("github.com/"):]
	}
	return s
}

// githubTokenURL rewrites a repo URL to the token-bearing https clone form.
func githubTokenURL(repo, token string) string {
	return "https://x-access-token:" + token + "@github.com/" + repoSlug(repo) + ".git"
}
