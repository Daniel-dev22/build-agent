package main

// engine runs one build invocation end to end: clone repo@ref (or extract an
// uploaded context), ensure the persistent buildx builder, validate/bust registry
// caches, build each service via `docker buildx` under fast/slow lane semaphores,
// rotate + version tags, prune retention, and bounded-GC. State mutations on the
// Build flow back to the controller + fleet via the onChange callback supplied by
// the registry.

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
// to amd64+arm64. The agent defaults auto→multi-arch because builds are pushed to
// a registry that serves both architectures.
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
	start := time.Now()
	cmd := exec.CommandContext(ctx, "docker", "buildx", "prune", "--builder", e.cfg.Builder, "--force",
		"--reserved-space", "10GB", "--max-used-space", "40GB", "--min-free-space", "20GB",
		"--filter", "until=168h")
	out, err := cmd.CombinedOutput()
	// Always log the duration. This step cost one build 155.7 seconds while
	// emitting nothing at all, so its only trace was a gap between two timestamps
	// in a table nobody reads. A prune that starts taking minutes should be
	// visible before it is load-bearing again.
	if err != nil {
		slog.Warn("bounded gc", "error", err, "took", time.Since(start), "out", strings.TrimSpace(string(out)))
		return
	}
	slog.Info("bounded gc", "took", time.Since(start))
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
	args := buildxArgs(e.cfg.Builder, d, svc, full, dockerfile, versionTag, apkBust, secretFile)

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

// buildxArgs assembles the `docker buildx build ...` argument vector for one
// service. Pure (no exec) so the arg ordering — platform, cache, build-args,
// optional --target, optional ghtoken secret, and the trailing -t/-f/--push
// context — is unit-testable.
func buildxArgs(builder string, d Descriptor, svc ServiceSpec, full, dockerfile, versionTag, apkBust, secretFile string) []string {
	args := []string{"buildx", "build", "--builder", builder, "--progress=plain"}
	args = append(args, platformArgs(d)...)
	args = append(args,
		"--cache-from", "type=registry,ref="+full+":buildcache",
		"--cache-to", "type=registry,ref="+full+":buildcache,mode=max")
	args = append(args, renderBuildArgs(d, svc, versionTag, apkBust)...)
	if svc.Target != "" {
		args = append(args, "--target", svc.Target)
	}
	if secretFile != "" {
		args = append(args, "--secret", "id=ghtoken,src="+secretFile)
	}
	args = append(args, "-t", full+":latest", "-f", dockerfile, "--push", svc.buildContext())
	return args
}

// runPreBuild executes the descriptor's PreBuild commands sequentially in the
// build workdir, streaming each command's output into the build log (prefix
// "prebuild"). Commands run via `sh -c` so they can use shell features (pipes,
// $(git rev-parse ...), redirection). Bounded by GitTimeout per command — these
// are quick source-stamping steps, not builds. Returns on the first failure.
func (e *engine) runPreBuild(ctx context.Context, b *Build, cmds []string) error {
	for _, c := range cmds {
		cctx, cancel := context.WithTimeout(ctx, e.cfg.GitTimeout)
		cmd := exec.CommandContext(cctx, "sh", "-c", c)
		cmd.Dir = b.workdir()
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		err := e.stream(b, "prebuild", cmd, nil)
		cancel()
		if err != nil {
			return fmt.Errorf("%q: %w", c, err)
		}
	}
	return nil
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
	apkBust := time.Now().UTC().Format("2006-01") // coarse monthly cache-bust bucket
	services := selectServices(d, req.Services)

	b.setRunning(versionTag, registry, services)
	onChange(EventStarted)

	// dir holds the build context (clone / uploaded tar); the job root above it
	// holds anything that must NOT be shipped to buildkit as context (the token
	// secret). Removing the root cleans up both, whatever the exit path.
	dir := b.workdir()
	defer os.RemoveAll(b.jobRoot())

	fail := func(msg string) {
		b.markFailed(msg)
		onChange(EventFailed)
	}

	// Context source: an uploaded inline context extracts into the workdir;
	// otherwise the agent clones repo@ref. Token vend only applies to the clone
	// path — an uploaded context never needs git auth, so secretFile stays empty
	// and buildService skips the buildx secret.
	var secretFile string
	if req.ContextB64 != "" {
		if err := extractContextTarGz(req.ContextB64, dir); err != nil {
			fail("extract context: " + err.Error())
			return
		}
	} else {
		// Private repos vend a short-lived GitHub App installation token from the
		// controller; it is used for the clone URL and as a buildx secret for
		// `go mod download` of private modules. Never written into the clone.
		var cloneToken string
		if isPrivate(req, e.cfg.PrivateRepoOwners) {
			tok, err := vendGitHubToken(ctx, e.cc, e.cfg.ControlCenterURL, []string{repoSlug(req.Repo)})
			if err != nil {
				fail("github token vend: " + err.Error())
				return
			}
			cloneToken = tok.Token
			// Inside the job root so the deferred RemoveAll above always covers it:
			// a path outside it would survive a kill between this write and the
			// os.Remove below, leaving a live token on disk under WORK_DIR. It sits
			// beside — not inside — the context dir, so buildkit never receives it
			// as context; the Dockerfile gets it only via --secret id=ghtoken.
			secretFile = filepath.Join(b.jobRoot(), ".ghtoken")
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

	// Project pre-build hook: run the descriptor's PreBuild commands in the freshly
	// populated workdir before any service builds — e.g. a `make version` step that
	// stamps a generated version file the Dockerfile then copies in. Fail the whole
	// build if any command errors; a missing stamp would produce a broken image.
	if err := e.runPreBuild(ctx, b, d.PreBuild); err != nil {
		fail("pre-build: " + err.Error())
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
	finishBuild(snap.State, onChange, func() { e.boundedGC(ctx) })
}

// finishBuild emits the terminal event and THEN runs post-build housekeeping.
//
// The order is the whole point, so it lives in its own function with a test:
// boundedGC used to run first, which put a `docker buildx prune` on the critical
// path between "the image is built, tagged and pushed" and "anyone is told".
//
// Measured on the frigate 0.19 build (job 0d886787, 2026-08-29): the service's own
// done event was emitted at 09:30:01.809 with the image already in the registry;
// the completed event did not leave the agent until 09:32:37.557. 155.7 seconds of
// dead air, during which control-center could not know the image was deployable.
// The event transport itself took 56ms and the controller's watcher tick 6.4s — so
// 96% of that build's 2m43s build-to-deploy handoff was this prune.
//
// It scales with how much cache the build churned, so it punishes exactly the big
// builds where a fast handoff matters most: 0.6-2.5s for a ~200-line control-center
// service, 155.7s for frigate.
//
// gc still runs synchronously afterwards — each build runs in its own goroutine
// (jobs.go) with no global build lock, so nothing waits on it, and the job context
// is not cancelled until run returns.
func finishBuild(state BuildState, onChange func(JobEvent), gc func()) {
	switch state {
	case BuildCompleted:
		onChange(EventCompleted)
		gc()
	case BuildCancelled:
		onChange(EventCancelled)
	default:
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

// isPrivate decides whether to vend a token. Explicit req.Private wins; otherwise
// the repo is inferred private when its URL names one of the configured owners
// (PRIVATE_REPO_OWNERS). An empty owner list means nothing is inferred private.
func isPrivate(req BuildRequest, owners []string) bool {
	if req.Private != nil {
		return *req.Private
	}
	for _, o := range owners {
		if o == "" {
			continue
		}
		if strings.Contains(req.Repo, o+"/") {
			return true
		}
	}
	return false
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
