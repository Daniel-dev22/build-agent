package main

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the build-agent runtime config, loaded from env at startup.
//
// The agent holds no client certificate. Transport security (mTLS) is expected
// to be terminated by a reverse proxy in front of it and attached on the way out
// by the proxy the agent dials; the agent's own application-layer identity to the
// controller is the per-host BearerToken.
type Config struct {
	NodeName string // short hostname this agent runs on (e.g. "builder01")
	SiteID   string // logical site/region this node belongs to (e.g. "site-a")

	ConfigDir string // persistent state dir (events.sqlite, job logs)
	WorkDir   string // scratch dir for cloned repos + build contexts

	ControlCenterURL string // base URL of the controller, e.g. https://controller.example.com:1443
	TraefikDockerDNS string // docker DNS name of the local reverse proxy; "" = dial the URL host directly
	TraefikDialPort  string // port on the resolved proxy IP (default "1443")

	BearerTokenFile string // per-host bearer token file (mode 0600, bind-mounted)
	BearerToken     string // populated at startup from BearerTokenFile

	// Builder / buildx
	Builder              string        // persistent buildx builder name (default "build-agent-builder")
	BuildkitdConfig      string        // optional path to buildkitd.toml (GC bounds); "" = none
	FastLane             int           // concurrent fast-lane (Go) service builds
	SlowLane             int           // concurrent slow-lane (frontend) service builds
	RetentionKeepDefault int           // default retention when a descriptor omits it
	GitTimeout           time.Duration // per-clone timeout
	BuildTimeout         time.Duration // hard ceiling per build job

	// MemoryCeiling (BUILD_MEMORY_CEILING, e.g. "7g") is the hard memory cap on the persistent
	// BuildKit builder (memory = memory-swap) AND the budget that per-service manifest `memory`
	// reservations are admitted against. 0 = unset: no cap, no reservations (previous behaviour).
	MemoryCeiling int64
	// MemoryDefault (BUILD_MEMORY_DEFAULT, default "1g") is the reservation charged to a service
	// whose manifest sets no `memory`. Only meaningful when a ceiling is set.
	MemoryDefault int64

	// SetupTimeout (BUILD_SETUP_TIMEOUT, default 20m) bounds a job's non-admission phases (token vend,
	// clones, builder ensure, pre_build, cache validation). Admission has no deadline by design;
	// BUILD_TIMEOUT starts when a service is admitted. DockerTimeout (BUILD_DOCKER_TIMEOUT, default
	// 15m) bounds every single docker CLI call the agent makes outside a build (builder
	// inspect/create/repair, post-build prune), so a wedged daemon cannot hold builderMu forever.
	SetupTimeout  time.Duration
	DockerTimeout time.Duration

	// PrivateRepoOwners lists GitHub owners/orgs whose repos are treated as
	// private (⇒ vend an installation token) when BuildRequest.Private is unset.
	// Empty (the default) ⇒ nothing is inferred private.
	PrivateRepoOwners []string
}

func loadConfig() Config {
	cfg := Config{
		NodeName: requireEnv("NODE_NAME"),
		SiteID:   requireEnv("SITE_ID"),

		ConfigDir: getEnv("CONFIG_DIR", "/var/lib/build-agent"),
		WorkDir:   getEnv("WORK_DIR", "/var/lib/build-agent/work"),

		ControlCenterURL: requireEnv("CONTROL_CENTER_URL"),
		// Empty default — dial the controller URL's own host. Hosts that reach the
		// controller through a local reverse-proxy container set this to that
		// container's DNS name to opt into rewrite mode (see network.go). A
		// non-empty default would break every host that dials directly.
		TraefikDockerDNS: getEnv("TRAEFIK_DOCKER_DNS", ""),
		TraefikDialPort:  getEnv("TRAEFIK_DIAL_PORT", "1443"),

		BearerTokenFile: getEnv("BEARER_TOKEN_FILE", "/etc/build-agent/bearer-token"),

		Builder:              getEnv("BUILDX_BUILDER", "build-agent-builder"),
		BuildkitdConfig:      getEnv("BUILDKITD_CONFIG", ""),
		FastLane:             getEnvInt("BUILD_FAST_LANE", 2),
		SlowLane:             getEnvInt("BUILD_SLOW_LANE", 1),
		RetentionKeepDefault: getEnvInt("REGISTRY_RETENTION_KEEP", 15),
		GitTimeout:           getEnvDuration("GIT_TIMEOUT", 5*time.Minute),
		BuildTimeout:         getEnvDuration("BUILD_TIMEOUT", 60*time.Minute),
		SetupTimeout:         getEnvDuration("BUILD_SETUP_TIMEOUT", 20*time.Minute),
		DockerTimeout:        getEnvDuration("BUILD_DOCKER_TIMEOUT", 15*time.Minute),
		PrivateRepoOwners:    getEnvList("PRIVATE_REPO_OWNERS"),
	}
	cfg.MemoryCeiling = requireBytes("BUILD_MEMORY_CEILING", "")
	cfg.MemoryDefault = requireBytes("BUILD_MEMORY_DEFAULT", "1g")
	tok, err := readBearerToken(cfg.BearerTokenFile)
	if err != nil {
		slog.Error("bearer token unreadable — cannot authenticate to controller",
			"error", err, "path", cfg.BearerTokenFile)
		os.Exit(1)
	}
	cfg.BearerToken = tok
	return cfg
}

func readBearerToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", &emptyTokenErr{path: path}
	}
	return tok, nil
}

type emptyTokenErr struct{ path string }

func (e *emptyTokenErr) Error() string { return "bearer token file " + e.path + " is empty" }

func requireEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		slog.Error("required env var not set", "var", k)
		os.Exit(1)
	}
	return v
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// getEnvList parses a comma-separated env var into a slice, trimming whitespace
// and dropping empty elements. An unset/blank var yields a nil slice.
func getEnvList(k string) []string {
	var out []string
	for _, part := range strings.Split(os.Getenv(k), ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getEnvInt(k string, def int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("invalid int env var, using default", "var", k, "value", v, "default", def)
		return def
	}
	return n
}

// requireBytes parses a size env var. A malformed value is fatal, unlike getEnvInt's fallback:
// silently falling back here would run builds with NO memory cap on a host the cap exists to protect.
func requireBytes(k, def string) int64 {
	v := getEnv(k, def)
	n, err := parseBytes(v)
	if err != nil {
		slog.Error("invalid size env var", "var", k, "value", v, "error", err)
		os.Exit(1)
	}
	return n
}

func getEnvDuration(k string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("invalid duration env var, using default", "var", k, "value", v, "default", def)
		return def
	}
	return d
}
