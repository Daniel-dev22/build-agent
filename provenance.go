package main

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// Image provenance: the labels every build stamps so a running container can
// answer "what source is this?" without any side-channel state.
//
// The agent is the ONLY place that knows all four facts at once. The image tag is
// a timestamp, the controller's pending_image_deploys row does not store the ref,
// and finished build jobs are evicted from memory — so if the image itself does
// not carry them, the answer is unrecoverable the moment the job ages out.
//
// Three of the labels are OCI-standard or namespaced equivalents; the fourth,
// build.context, exists because it is the only one that is ALWAYS knowable. An
// uploaded-context build (genmon: `tar | base64` from the ansible tree, no repo
// and no ref on the wire) can populate none of the other three, and without
// build.context its image would be indistinguishable from an upstream image the
// agent never touched. Consumers asking "did we build this?" must read
// build.context, NOT build.ref.
const (
	labelSource   = "org.opencontainers.image.source"
	labelRevision = "org.opencontainers.image.revision"
	labelRef      = "com.controlcenter.build.ref"
	labelContext  = "com.controlcenter.build.context"

	contextGit    = "git"
	contextUpload = "upload"

	// A label value longer than this is a caller bug, not a ref. Dropped rather
	// than truncated: half a URL is worse than no URL, because it still reads as
	// an answer.
	maxLabelValue = 512
)

// provenance is what one build knows about its own source. Zero values mean
// "unknown" and are omitted from the argv entirely — never emitted as an empty
// label, which would read downstream as "a branch named empty string".
type provenance struct {
	Context  string // contextGit | contextUpload — always set
	Repo     string // clone URL, credentials stripped
	Ref      string // tag / branch as requested
	Revision string // resolved commit sha
}

// provenanceFor derives the source-independent part of the provenance from the
// request. Revision is filled in later (it is only knowable after the clone).
func provenanceFor(req BuildRequest) provenance {
	if req.ContextB64 != "" {
		// An uploaded context has no repo and no ref on the wire; anything the
		// request happens to carry in those fields did not produce this tree.
		return provenance{Context: contextUpload}
	}
	return provenance{
		Context: contextGit,
		Repo:    scrubURLCredentials(req.Repo),
		Ref:     req.Ref,
	}
}

// labelArgs renders the provenance as buildx --label flags, in a fixed order so
// the argv is deterministic and testable. Values that do not survive
// sanitizeLabelValue are omitted along with their key.
func (p provenance) labelArgs() []string {
	pairs := [][2]string{
		{labelSource, p.Repo},
		{labelRevision, p.Revision},
		{labelRef, p.Ref},
		{labelContext, p.Context},
	}
	args := make([]string, 0, 2*len(pairs))
	for _, kv := range pairs {
		if v := sanitizeLabelValue(kv[1]); v != "" {
			args = append(args, "--label", kv[0]+"="+v)
		}
	}
	return args
}

// sanitizeLabelValue returns the value to stamp, or "" to omit the label.
//
// Values reach here from the build request, i.e. from whatever posted to the
// agent. They are placed in an argv (never a shell), so there is no injection
// route — but a control character would still be written verbatim into the image
// config, corrupting anything that later reads the labels as lines. A value with
// one is dropped rather than repaired: a silently-edited ref is a wrong answer,
// and this label's whole purpose is to be trusted.
func sanitizeLabelValue(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxLabelValue {
		return ""
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return s
}

// scrubURLCredentials removes any userinfo from a URL-form repo so a credential
// can never be baked into a published image label.
//
// req.Repo is the caller's URL and the token-bearing form is built locally inside
// gitClone, so today nothing reaches here with a secret in it. This is defence in
// depth against that ordering ever changing: the cost of being wrong is a token
// published in an image that is then pushed to a registry, which cannot be
// retracted by deleting the tag.
//
// scp-like syntax (git@github.com:owner/repo.git) is left alone — its "git@" is
// the ssh user, carries no secret, and removing it would break the URL as a clone
// target for anything that later reads the label.
func scrubURLCredentials(repo string) string {
	repo = strings.TrimSpace(repo)
	if !strings.Contains(repo, "://") {
		return repo
	}
	u, err := url.Parse(repo)
	if err != nil {
		// Unparseable and scheme-bearing: it may still hold a credential, and we
		// cannot tell. Omit rather than guess.
		return ""
	}
	u.User = nil
	return u.String()
}

// gitRevision resolves the commit the clone actually landed on.
//
// Best effort by design: it runs AFTER a successful clone, so a failure here means
// git worked well enough to produce a tree but not to describe it. Labels are
// additive metadata — failing the build over a missing one would let a bookkeeping
// step veto the thing it is bookkeeping for. The label is omitted and the build
// proceeds.
//
// One fork per build, against a build measured in minutes. The in-process
// alternative (parsing .git/HEAD, then refs/heads or packed-refs) is more code and
// more failure modes to save an immeasurable amount of wall clock.
func (e *engine) gitRevision(ctx context.Context, b *Build, dir string) string {
	cctx, cancel := context.WithTimeout(ctx, e.cfg.GitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-C", dir, "rev-parse", "HEAD")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		b.appendLine("[git] rev-parse HEAD failed, omitting the revision label: " + err.Error())
		return ""
	}
	return strings.TrimSpace(string(out))
}
