package main

// Process-global pooled HTTP client for every agent → controller call (event push
// via the kit outbox, reconcile, GitHub installation-token vend).
//
// Auth model: transport security (mTLS) is expected to be handled by the reverse
// proxies on each side; the agent presents NO client cert. Its own per-node
// identity is the BearerToken header attached by the kit bearer round-tripper (agent-kit-go/transport), which also refuses
// cross-origin redirects so the token and body never follow one.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Daniel-dev22/agent-kit-go/transport"
)

func readErrorBody(r io.Reader) string {
	const max = 4 * 1024
	buf := make([]byte, max)
	n, _ := io.ReadFull(io.LimitReader(r, max), buf)
	if n == 0 {
		return ""
	}
	return string(buf[:n])
}

// buildControlCenterClient returns the pooled HTTP client. Direct mode
// (TraefikDockerDNS=="") dials the URL host normally; rewrite mode
// (TraefikDockerDNS!="") rewrites the dial target to a local reverse-proxy
// container while keeping the URL host as the Host header and TLS SNI, so that
// proxy can match its routing rule and attach the client certificate on the way
// out. The agent itself never holds a certificate.
func buildControlCenterClient(cfg Config) *http.Client {
	return transport.NewRouterClient(transport.RouterClientConfig{
		TraefikDockerDNS: cfg.TraefikDockerDNS,
		TraefikDialPort:  cfg.TraefikDialPort,
		BearerToken:      cfg.BearerToken,
	})
}

// githubTokenResp is the controller's response to POST /api/build/github-token:
// a short-lived GitHub App installation token the agent uses for `git clone` and
// (as a buildx secret) `go mod download` of private modules.
type githubTokenResp struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// vendGitHubToken asks the controller to mint an installation token for the
// given repos. Returns ("", nil) is never used — a missing token is an error;
// callers only invoke this for private builds. Transient failures retry within
// ctx; auth/permission failures short-circuit.
func vendGitHubToken(ctx context.Context, client *http.Client, controlCenterURL string, repos []string) (githubTokenResp, error) {
	if client == nil || controlCenterURL == "" {
		return githubTokenResp{}, fmt.Errorf("controller client/url not configured")
	}
	body, _ := json.Marshal(map[string]any{"repositories": repos})
	url := controlCenterURL + "/api/build/github-token"

	const maxAttempts = 4
	backoff := 500 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return githubTokenResp{}, fmt.Errorf("vend ctx done before retry: %w", ctx.Err())
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		tok, transient, err := vendGitHubTokenOnce(ctx, client, url, body)
		if err == nil {
			return tok, nil
		}
		if !transient {
			return githubTokenResp{}, err
		}
		lastErr = err
	}
	return githubTokenResp{}, fmt.Errorf("github-token vend transient failure after %d attempts: %w", maxAttempts, lastErr)
}

func vendGitHubTokenOnce(ctx context.Context, client *http.Client, url string, body []byte) (githubTokenResp, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return githubTokenResp{}, false, fmt.Errorf("build vend request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return githubTokenResp{}, true, fmt.Errorf("call controller: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return githubTokenResp{}, false, fmt.Errorf("controller rejected token vend (%d): %s", resp.StatusCode, readErrorBody(resp.Body))
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusInternalServerError:
		return githubTokenResp{}, true, fmt.Errorf("controller transient %d: %s", resp.StatusCode, readErrorBody(resp.Body))
	default:
		return githubTokenResp{}, false, fmt.Errorf("controller returned %d: %s", resp.StatusCode, readErrorBody(resp.Body))
	}
	var t githubTokenResp
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return githubTokenResp{}, false, fmt.Errorf("decode vend response: %w", err)
	}
	if t.Token == "" {
		return githubTokenResp{}, false, fmt.Errorf("vend response missing token")
	}
	return t, false, nil
}
