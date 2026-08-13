package main

// Registry-side housekeeping — buildcache validation/bust + timestamp-tag
// retention — done directly over the registry v2 HTTP API (much faster than
// shelling out per request). Tag rotation + versioned tagging stay on
// `docker buildx imagetools` (os/exec) since they re-publish manifests.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"time"
)

var (
	tsTagRe       = regexp.MustCompile(`^\d{8}-\d{6}$`)
	protectedTags = map[string]struct{}{"latest": {}, "stable": {}, "buildcache": {}}
)

const manifestAccept = "application/vnd.oci.image.index.v1+json," +
	"application/vnd.oci.image.manifest.v1+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.docker.distribution.manifest.v2+json"

// registryClient performs UNAUTHENTICATED v2 API calls against the target
// registry, over the same network path buildx pushes through. It sends no
// credentials, so the registry must either allow anonymous read+delete from this
// host or these best-effort calls simply no-op.
type registryClient struct {
	hc *http.Client
}

func newRegistryClient() *registryClient {
	return &registryClient{hc: &http.Client{Timeout: 20 * time.Second}}
}

func (r *registryClient) base(registry, image string) string {
	return fmt.Sprintf("https://%s/v2/%s", registry, image)
}

// validateCache returns "none" (no buildcache), "valid", or ("corrupted", n) when
// n manifest blobs are missing — the agent then busts and rebuilds from scratch.
func (r *registryClient) validateCache(ctx context.Context, registry, image string) (status string, missing int) {
	base := r.base(registry, image)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/manifests/buildcache", nil)
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	resp, err := r.hc.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return "none", 0
	}
	var m struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	dec := json.NewDecoder(resp.Body)
	derr := dec.Decode(&m)
	resp.Body.Close()
	if derr != nil {
		return "valid", 0
	}
	digests := map[string]struct{}{}
	if m.Config.Digest != "" {
		digests[m.Config.Digest] = struct{}{}
	}
	for _, l := range m.Layers {
		if l.Digest != "" {
			digests[l.Digest] = struct{}{}
		}
	}
	if len(digests) == 0 {
		return "valid", 0
	}
	for d := range digests {
		hreq, _ := http.NewRequestWithContext(ctx, http.MethodHead, base+"/blobs/"+d, nil)
		hresp, herr := r.hc.Do(hreq)
		if herr != nil || hresp.StatusCode != http.StatusOK {
			missing++
		}
		if hresp != nil {
			hresp.Body.Close()
		}
	}
	if missing > 0 {
		return "corrupted", missing
	}
	return "valid", 0
}

// resolveDigest returns the Docker-Content-Digest for a tag (index or manifest),
// or "" if absent/unreachable — what DELETE must target to drop the whole tag.
func (r *registryClient) resolveDigest(ctx context.Context, base, ref string) string {
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, base+"/manifests/"+ref, nil)
	req.Header.Set("Accept", manifestAccept)
	resp, err := r.hc.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	return resp.Header.Get("Docker-Content-Digest")
}

// deleteManifest DELETEs a manifest by digest. 2xx (deleted) and 404 (already
// gone) both count as success; other statuses are logged and return false.
func (r *registryClient) deleteManifest(ctx context.Context, base, digest string) bool {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, base+"/manifests/"+digest, nil)
	resp, err := r.hc.Do(req)
	if err != nil {
		slog.Warn("registry DELETE failed", "digest", digest, "error", err)
		return false
	}
	defer resp.Body.Close()
	if (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusNotFound {
		return true
	}
	slog.Warn("registry DELETE non-2xx", "digest", digest, "status", resp.StatusCode)
	return false
}

func (r *registryClient) bustCache(ctx context.Context, registry, image string) {
	base := r.base(registry, image)
	if d := r.resolveDigest(ctx, base, "buildcache"); d != "" {
		if r.deleteManifest(ctx, base, d) {
			slog.Info("busted corrupted buildcache", "image", image)
		}
	}
}

// pruneOldTags keeps the newest keep timestamp tags (YYYYMMDD-HHMMSS) and DELETEs
// the rest (deduped by digest). latest/stable/buildcache + semver are protected.
// No-op when keep<=0. Best-effort: a failure never fails the build.
func (r *registryClient) pruneOldTags(ctx context.Context, registry, image string, keep int) {
	if keep <= 0 {
		return
	}
	base := r.base(registry, image)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/tags/list", nil)
	resp, err := r.hc.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return
	}
	var tl struct {
		Tags []string `json:"tags"`
	}
	derr := json.NewDecoder(resp.Body).Decode(&tl)
	resp.Body.Close()
	if derr != nil {
		return
	}
	var candidates []string
	for _, t := range tl.Tags {
		if _, prot := protectedTags[t]; prot {
			continue
		}
		if tsTagRe.MatchString(t) {
			candidates = append(candidates, t)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(candidates)))
	if len(candidates) <= keep {
		return
	}
	stale := candidates[keep:]
	seen := map[string]struct{}{}
	deleted := 0
	for _, t := range stale {
		d := r.resolveDigest(ctx, base, t)
		if d == "" {
			continue
		}
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		if r.deleteManifest(ctx, base, d) {
			deleted++
		}
	}
	slog.Info("registry retention pruned", "image", image, "deleted", deleted, "kept", keep, "stale_tags", len(stale))
}
