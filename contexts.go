package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Named build contexts.
//
// A descriptor may declare extra git repos the Dockerfile consumes through buildx's named contexts
// (`COPY --from=kit ...`, `FROM kit`):
//
//	"contexts": {"kit": {"repo": "git@github.com:org/android-kit.git", "ref": "0.4.0"}}
//
// The agent clones each one itself with the job's vended token (gitclone.go: header auth, never in
// the URL), resolves its ref to a commit SHA, and passes `--build-context kit=<dir>`. No credential
// enters BuildKit: a Dockerfile that used to clone the second repo over a forwarded SSH key just
// reads the context. The request may pin a different ref per name (BuildRequest.ContextRefs) so a
// release can name the exact kit revision; the repo itself is fixed by the descriptor.

// ContextSpec is one named context in the descriptor.
type ContextSpec struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref,omitempty"` // tag / branch / commit; empty = the repo's default branch
}

// ContextResult is what the job reports for a named context: the source it actually built from.
type ContextResult struct {
	Repo     string `json:"repo"`     // credentials stripped
	Ref      string `json:"ref,omitempty"`
	Revision string `json:"revision"` // resolved commit SHA
}

// contextNameRe bounds a context name to something safe in `--build-context name=dir` and in a path.
var contextNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// namedContext is a cloned context ready to hand to buildx.
type namedContext struct {
	name, dir string
}

func contextDirs(cs []namedContext) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.dir
	}
	return out
}

// sortedContextNames returns the declared names in a stable order, so argv and clone order are
// deterministic.
func sortedContextNames(m map[string]ContextSpec) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// validate checks the declared contexts. A repo starting with "-" is refused up front as well as by
// cloneArgs' "--": the clone is a fork of the agent's own git with a vended token in its env.
func validateContexts(contexts map[string]ContextSpec) error {
	for _, name := range sortedContextNames(contexts) {
		if !contextNameRe.MatchString(name) {
			return fmt.Errorf("contexts: name %q must match %s", name, contextNameRe)
		}
		c := contexts[name]
		if strings.TrimSpace(c.Repo) == "" || strings.HasPrefix(c.Repo, "-") {
			return fmt.Errorf("contexts.%s: repo is required and must not start with '-'", name)
		}
	}
	return nil
}

// validateContextRefs rejects a request ref override for a context the descriptor never declared.
func validateContextRefs(contexts map[string]ContextSpec, refs map[string]string) error {
	for name := range refs {
		if _, ok := contexts[name]; !ok {
			return fmt.Errorf("context_refs.%s: the descriptor declares no such context", name)
		}
	}
	return nil
}

// isPrivateRepo decides whether a clone of repo needs the vended token: the request's explicit
// Private flag wins, else the repo is private when it names a configured owner.
func isPrivateRepo(repo string, override *bool, owners []string) bool {
	if override != nil {
		return *override
	}
	for _, o := range owners {
		if o != "" && strings.Contains(repo, o+"/") {
			return true
		}
	}
	return false
}

// tokenRepos lists the "owner/repo" slugs of every private repo this job will clone (the app repo
// and the named contexts), in a stable order. Empty = no token is needed.
func tokenRepos(req BuildRequest, d Descriptor, owners []string) []string {
	var slugs []string
	if req.ContextB64 == "" && isPrivate(req, owners) {
		slugs = append(slugs, repoSlug(req.Repo))
	}
	for _, name := range sortedContextNames(d.Contexts) {
		if r := d.Contexts[name].Repo; isPrivateRepo(r, req.Private, owners) {
			slugs = append(slugs, repoSlug(r))
		}
	}
	return slugs
}

// cloneContexts clones every declared context into <jobRoot>/ctx-<name>, resolves each ref to a SHA
// and returns them for buildx plus the per-name result for the job. A context whose commit cannot be
// resolved fails the job: its SHA is the provenance a consumer asserts against, so an empty one is
// not "best effort" the way the app's label is.
func (e *engine) cloneContexts(ctx context.Context, b *Build, d Descriptor, req BuildRequest, token string) ([]namedContext, map[string]ContextResult, error) {
	if len(d.Contexts) == 0 {
		return nil, nil, nil
	}
	var cs []namedContext
	results := make(map[string]ContextResult, len(d.Contexts))
	for _, name := range sortedContextNames(d.Contexts) {
		spec := d.Contexts[name]
		ref := spec.Ref
		if r, ok := req.ContextRefs[name]; ok {
			ref = r
		}
		dir := filepath.Join(b.jobRoot(), "ctx-"+name)
		tok := ""
		if isPrivateRepo(spec.Repo, req.Private, e.cfg.PrivateRepoOwners) {
			tok = token
		}
		if err := e.gitClone(ctx, b, spec.Repo, ref, dir, tok); err != nil {
			return nil, nil, fmt.Errorf("git clone context %s: %w", name, err)
		}
		sha := e.gitRevision(ctx, b, dir)
		if sha == "" {
			return nil, nil, fmt.Errorf("context %s: could not resolve %q to a commit", name, ref)
		}
		cs = append(cs, namedContext{name: name, dir: dir})
		results[name] = ContextResult{Repo: normalizeRepoURL(spec.Repo), Ref: ref, Revision: sha}
	}
	return cs, results, nil
}

// buildExtras are the buildx arguments that exist only because of this phase, kept out of
// buildxArgs' positional list.
type buildExtras struct {
	contexts     []namedContext
	metadataFile string
}

// args renders them: `--build-context name=dir` per context, and --metadata-file so the pushed
// digest is read back from buildx instead of re-queried from the registry.
func (x buildExtras) args() []string {
	var out []string
	for _, c := range x.contexts {
		out = append(out, "--build-context", c.name+"="+c.dir)
	}
	if x.metadataFile != "" {
		out = append(out, "--metadata-file", x.metadataFile)
	}
	return out
}

var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// readPushedDigest reads containerimage.digest from a buildx --metadata-file.
func readPushedDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var meta struct {
		Digest string `json:"containerimage.digest"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	if !digestRe.MatchString(meta.Digest) {
		return "", fmt.Errorf("%s has no valid containerimage.digest (%q)", filepath.Base(path), meta.Digest)
	}
	return meta.Digest, nil
}
