package main

import (
	"context"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Wire contract: the POST /v1/builds body. The repo's build.json IS the
// descriptor; ansible adds repo/ref/registry/version/services/private on top.
// ---------------------------------------------------------------------------

// BuildRequest is the trigger body. Descriptor is the repo's build.json verbatim.
//
// The build context comes from EITHER Repo/Ref (the agent clones repo@ref) OR
// ContextB64 (the caller uploads the context inline). The latter is for projects
// whose Dockerfile/context isn't in a standalone cloneable repo — e.g. genmon,
// whose context lives in the ansible repo's genmon/image/ dir. Exactly one source
// is required.
type BuildRequest struct {
	Descriptor Descriptor `json:"descriptor"`
	Repo       string     `json:"repo"`                  // git URL (ssh or https form)
	Ref        string     `json:"ref"`                   // tag / branch / commit
	ContextB64 string     `json:"context_b64,omitempty"` // base64(gzip(tar)) of the build context — alternative to Repo/Ref
	Registry   string     `json:"registry"`              // overrides Descriptor default
	Version    string     `json:"version,omitempty"`     // required when version.scheme=external
	Services   []string   `json:"services,omitempty"`    // subset to build; empty = all
	Private    *bool      `json:"private,omitempty"`     // nil = infer from repo host/org
	TriggerKey string     `json:"trigger_key,omitempty"`
}

// Descriptor mirrors the per-repo build.json the Python orchestrator consumes.
type Descriptor struct {
	Project       string            `json:"project"`
	ImagePrefix   string            `json:"image_prefix"`
	BuildkitdConf string            `json:"buildkitd_config,omitempty"`
	Platform      PlatformSpec      `json:"platform"`
	Version       VersionSpec       `json:"version"`
	Lanes         LaneSpec          `json:"lanes"`
	TagRotation   *bool             `json:"tag_rotation,omitempty"` // default true
	RetentionKeep *int              `json:"retention_keep,omitempty"`
	BuildArgs     map[string]string `json:"build_args,omitempty"` // project-level, inherited
	// PreBuild runs these shell commands (via `sh -c`) in the clone workdir before
	// any service builds — e.g. frigate's `make version` equivalent that stamps
	// frigate/version.py + web/.env. Descriptors are authored by the trusted caller
	// (control-api), never by end users; the externally-influenced git ref is passed
	// to `git clone --branch`, never interpolated into these strings — so there is no
	// injection surface even though they run through a shell.
	PreBuild []string      `json:"pre_build,omitempty"`
	Services []ServiceSpec `json:"services"`
}

type PlatformSpec struct {
	Mode  string `json:"mode"`            // "auto" | "fixed"
	Value string `json:"value,omitempty"` // e.g. "linux/arm64" when fixed
}

type VersionSpec struct {
	Scheme string `json:"scheme"` // "timestamp" | "external"
}

type LaneSpec struct {
	Fast int `json:"fast"`
	Slow int `json:"slow"`
}

type ServiceSpec struct {
	Name       string            `json:"name"`
	Context    string            `json:"context,omitempty"`    // default "."
	Dockerfile string            `json:"dockerfile,omitempty"` // default "{name}/Dockerfile"
	Target     string            `json:"target,omitempty"`     // buildx --target stage; empty = final stage
	Lane       string            `json:"lane,omitempty"`       // "fast" | "slow"
	BuildArgs  map[string]string `json:"build_args,omitempty"`
}

func (s ServiceSpec) dockerfile() string {
	if s.Dockerfile != "" {
		return s.Dockerfile
	}
	return s.Name + "/Dockerfile"
}

func (s ServiceSpec) buildContext() string {
	if s.Context != "" {
		return s.Context
	}
	return "."
}

func (s ServiceSpec) lane() string {
	if s.Lane == "slow" {
		return "slow"
	}
	return "fast"
}

// ---------------------------------------------------------------------------
// Job model — one build invocation (may build N services).
// ---------------------------------------------------------------------------

type BuildState string

const (
	BuildPending   BuildState = "pending"
	BuildRunning   BuildState = "running"
	BuildCompleted BuildState = "completed"
	BuildFailed    BuildState = "failed"
	BuildCancelled BuildState = "cancelled"
)

type ServiceState string

const (
	SvcPending  ServiceState = "pending"
	SvcBuilding ServiceState = "building"
	SvcDone     ServiceState = "done"
	SvcFailed   ServiceState = "failed"
)

// JobEvent mirrors the kit/controller lifecycle vocabulary.
type JobEvent string

const (
	EventStarted   JobEvent = "started"
	EventProgress  JobEvent = "progress"
	EventCompleted JobEvent = "completed"
	EventFailed    JobEvent = "failed"
	EventCancelled JobEvent = "cancelled"
)

// JobEventHook is registered by events.go to receive lifecycle notifications.
type JobEventHook func(b *Build, evt JobEvent)

const (
	ringBufferSize     = 2000 // log lines kept in memory per build
	recentJobsRetained = 50
	subscriberBuffer   = 256
)

// ServiceProgress is the per-service slice of a build's progress.
type ServiceProgress struct {
	Name       string       `json:"name"`
	Lane       string       `json:"lane"`
	State      ServiceState `json:"state"`
	Attempts   int          `json:"attempts,omitempty"`
	StartedAt  *time.Time   `json:"started_at,omitempty"`
	EndedAt    *time.Time   `json:"ended_at,omitempty"`
	DurationMs int64        `json:"duration_ms,omitempty"`
	Image      string       `json:"image,omitempty"`
	Tag        string       `json:"tag,omitempty"`
	Err        string       `json:"error,omitempty"`
	// Live docker buildx sub-step (parsed from --progress=plain `#N [stage n/m]`
	// lines). A liveness indicator only — meaningful while State==building;
	// cleared on finish. Total is per buildkit stage, not the whole build.
	CurrentStep int    `json:"current_step,omitempty"`
	TotalSteps  int    `json:"total_steps,omitempty"`
	StepName    string `json:"step_name,omitempty"`
}

// BuildProgress is the rolled-up progress shown on the job card.
type BuildProgress struct {
	TotalServices  int       `json:"total_services"`
	DoneServices   int       `json:"done_services"`
	FailedServices int       `json:"failed_services"`
	CurrentService string    `json:"current_service,omitempty"`
	Percent        float64   `json:"percent"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// buildPublic carries the JSON-serializable fields of a Build, split out so
// snapshot() can return a lock-free copy without copying the mutex.
type buildPublic struct {
	ID          string            `json:"id"`
	Project     string            `json:"project"`
	Ref         string            `json:"ref,omitempty"`
	VersionTag  string            `json:"version_tag,omitempty"`
	Registry    string            `json:"registry,omitempty"`
	State       BuildState        `json:"state"`
	Services    []ServiceProgress `json:"services"`
	Progress    *BuildProgress    `json:"progress,omitempty"`
	StartedAt   time.Time         `json:"started_at"`
	CompletedAt time.Time         `json:"completed_at,omitempty"`
	ExitCode    int               `json:"exit_code"`
	ErrorMsg    string            `json:"error,omitempty"`
	LineCount   int               `json:"line_count"`
	TriggerKey  string            `json:"trigger_key,omitempty"`
}

// Build is one build invocation tracked through its lifecycle.
type Build struct {
	buildPublic

	mu          sync.Mutex
	cancel      context.CancelFunc
	dir         string // per-build clone/context dir under cfg.WorkDir
	ringBuffer  []string
	subscribers map[chan string]struct{}
}

func (b *Build) workdir() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dir
}

// recomputeProgressLocked rolls per-service states up into Progress. Caller holds b.mu.
func (b *Build) recomputeProgressLocked() {
	total := len(b.Services)
	done, failed := 0, 0
	current := ""
	for _, s := range b.Services {
		switch s.State {
		case SvcDone:
			done++
		case SvcFailed:
			failed++
		case SvcBuilding:
			if current == "" {
				current = s.Name
			}
		}
	}
	pct := 0.0
	if total > 0 {
		pct = float64(done+failed) / float64(total) * 100
	}
	b.Progress = &BuildProgress{
		TotalServices: total, DoneServices: done, FailedServices: failed,
		CurrentService: current, Percent: pct, UpdatedAt: time.Now().UTC(),
	}
}

// setRunning marks the build running, seeds per-service progress (all pending).
func (b *Build) setRunning(versionTag, registry string, services []ServiceSpec) {
	b.mu.Lock()
	b.State = BuildRunning
	b.StartedAt = time.Now().UTC()
	b.VersionTag = versionTag
	b.Registry = registry
	b.Services = make([]ServiceProgress, len(services))
	for i, s := range services {
		b.Services[i] = ServiceProgress{Name: s.Name, Lane: s.lane(), State: SvcPending}
	}
	b.recomputeProgressLocked()
	b.mu.Unlock()
}

// setServiceStep updates the live buildx sub-step of a building service. Returns
// true only when the step actually changed, so callers broadcast at most once
// per real step transition (the parser sees many lines per step). State is left
// untouched — this is a liveness annotation, not a lifecycle transition.
func (b *Build) setServiceStep(name string, current, total int, stepName string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.Services {
		if b.Services[i].Name != name {
			continue
		}
		if b.Services[i].State != SvcBuilding {
			return false
		}
		if b.Services[i].CurrentStep == current && b.Services[i].TotalSteps == total &&
			b.Services[i].StepName == stepName {
			return false
		}
		b.Services[i].CurrentStep = current
		b.Services[i].TotalSteps = total
		b.Services[i].StepName = stepName
		return true
	}
	return false
}

func (b *Build) setServiceState(name string, st ServiceState, mutate func(*ServiceProgress)) {
	b.mu.Lock()
	for i := range b.Services {
		if b.Services[i].Name == name {
			b.Services[i].State = st
			if mutate != nil {
				mutate(&b.Services[i])
			}
			break
		}
	}
	b.recomputeProgressLocked()
	b.mu.Unlock()
}

// finalize sets the terminal state from per-service outcomes (unless cancelled).
func (b *Build) finalize() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.State == BuildCancelled {
		return
	}
	failed := 0
	for _, s := range b.Services {
		if s.State == SvcFailed {
			failed++
		}
	}
	b.CompletedAt = time.Now().UTC()
	if failed > 0 {
		b.State = BuildFailed
		b.ExitCode = 1
		if b.ErrorMsg == "" {
			b.ErrorMsg = "one or more services failed to build"
		}
	} else {
		b.State = BuildCompleted
		b.ExitCode = 0
	}
}

func (b *Build) markFailed(msg string) {
	b.mu.Lock()
	if b.State != BuildCancelled {
		b.State = BuildFailed
		b.ExitCode = 1
		b.ErrorMsg = msg
		b.CompletedAt = time.Now().UTC()
	}
	b.mu.Unlock()
}

func (b *Build) snapshot() buildPublic {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Deep-copy the services slice so callers can't race on the backing array.
	cp := b.buildPublic
	cp.Services = append([]ServiceProgress(nil), b.buildPublic.Services...)
	if b.buildPublic.Progress != nil {
		p := *b.buildPublic.Progress
		cp.Progress = &p
	}
	return cp
}

func (b *Build) appendLine(line string) {
	b.mu.Lock()
	b.ringBuffer = append(b.ringBuffer, line)
	if len(b.ringBuffer) > ringBufferSize {
		b.ringBuffer = b.ringBuffer[len(b.ringBuffer)-ringBufferSize:]
	}
	b.LineCount++
	subs := make([]chan string, 0, len(b.subscribers))
	for c := range b.subscribers {
		subs = append(subs, c)
	}
	b.mu.Unlock()
	for _, c := range subs {
		select {
		case c <- line:
		default:
			select {
			case <-c:
			default:
			}
			select {
			case c <- line:
			default:
			}
		}
	}
}

func (b *Build) logLines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.ringBuffer...)
}

func tPtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
