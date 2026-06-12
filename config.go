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
// The agent reaches controller exactly like duplicacy/gdrive: transport mTLS
// is attached by the local docker Traefik (NAS) or k3s Traefik (cluster node);
// app-layer identity is the per-host BearerToken. It NEVER holds a client cert.
type Config struct {
	NodeName string // hostname-short this agent runs on (e.g. "nas")
	SiteID   string // "kd" | "ng"

	ConfigDir string // persistent state dir (events.sqlite, job logs)
	WorkDir   string // scratch dir for cloned repos + build contexts

	ControlCenterURL string // https://controller-api.<domain>:1443
	TraefikDockerDNS string // docker DNS name → local Traefik ("traefik" on NAS; "" = direct on k3s)
	TraefikDialPort  string // port on the resolved Traefik IP (default "1443")

	BearerTokenFile string // per-host bearer token file (mode 0600, bind-mounted)
	BearerToken     string // populated at startup from BearerTokenFile

	// Builder / buildx
	Builder         string        // persistent buildx builder name (default "build-agent-builder")
	BuildkitdConfig string        // optional path to buildkitd.toml (GC bounds); "" = none
	FastLane        int           // concurrent fast-lane (Go) service builds
	SlowLane        int           // concurrent slow-lane (frontend) service builds
	RetentionKeepDefault int      // default retention when a descriptor omits it
	GitTimeout      time.Duration // per-clone timeout
	BuildTimeout    time.Duration // hard ceiling per build job
}

func loadConfig() Config {
	cfg := Config{
		NodeName: requireEnv("NODE_NAME"),
		SiteID:   requireEnv("SITE_ID"),

		ConfigDir: getEnv("CONFIG_DIR", "/var/lib/build-agent"),
		WorkDir:   getEnv("WORK_DIR", "/var/lib/build-agent/work"),

		ControlCenterURL: requireEnv("CONTROL_CENTER_URL"),
		// Empty default — k3s nodes use direct mode; NAS hosts set "traefik" to
		// opt into rewrite mode (see network.go). A non-empty default would break
		// k3s nodes the way it did for the other agents.
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
	}
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
