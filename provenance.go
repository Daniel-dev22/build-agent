package main

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
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

	// Enough of git's own message to name the cause; a runaway stderr must not
	// flood the build's ring buffer and evict the build output around it.
	maxLoggedStderr = 200

	// rev-parse reads one file in a clone that already exists; it is not a clone.
	// Reusing GitTimeout (5 min) would let a wedged git stall the build for five
	// minutes for a label this file calls best-effort.
	gitRevParseTimeout = 15 * time.Second
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
		Repo:    normalizeRepoURL(req.Repo),
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
		if !allowedLabelRune(r) {
			return ""
		}
	}
	return s
}

// allowedLabelRune rejects every rune that can corrupt or spoof a label read back
// as a line of text. C0 and DEL are the obvious half; review added the rest,
// which are the ones that survive a naive filter: the C1 block, the Unicode line
// and paragraph separators, and the bidi overrides that let a rendered ref read
// as a different ref than the one stored.
func allowedLabelRune(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f:
		return false
	case r >= 0x80 && r <= 0x9f: // C1
		return false
	case r == 0x2028 || r == 0x2029: // LINE / PARAGRAPH SEPARATOR
		return false
	case r >= 0x202a && r <= 0x202e: // bidi embedding / override
		return false
	case r >= 0x2066 && r <= 0x2069: // bidi isolates
		return false
	case r == 0xfeff: // zero-width no-break space
		return false
	}
	return true
}

// normalizeRepoURL turns a clone URL into the value stamped as
// org.opencontainers.image.source, or "" to omit the label.
//
// It does two jobs that were previously one, because review found the first was
// incomplete and the second was missing entirely.
//
// 1. NO CREDENTIAL MAY REACH A PUSHED IMAGE. The predecessor only cleared
// url.User, which leaks in four measured forms: a transport-helper prefix
// (git::https://tok@host/…, ext::…) makes url.Parse yield an Opaque URL so
// clearing User is a no-op, and a credential in the ?query or #fragment is not
// userinfo at all. An image cannot be un-published by deleting its tag, so
// anything not reducible to a recognised, credential-free shape is DROPPED.
//
// 2. THE VALUE MUST BE A URL, because this key already has a consumer.
// docker-agent's sourceRepoFromLabels reads org.opencontainers.image.source to
// decide an image's update strategy, and it parses "owner/repo" out of an https
// GitHub URL. The estate configures every repo in scp syntax
// (git@github.com:Daniel-dev22/build-agent.git), which that parser accepts as a
// slug verbatim — one "/" — and would then query a repository that cannot exist,
// flipping a working digest comparison into a hard update failure. Emitting the
// OCI-conformant https form (the spec says "URL to get source code") makes the
// existing parser correct rather than merely unbroken. The .git suffix is dropped
// for the same reason: nothing downstream strips it.
func normalizeRepoURL(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return ""
	}
	// A git transport helper ("git::", "ext::", …) can carry an arbitrary nested
	// URL or an executable remote helper. Not reducible to a source URL, and the
	// place to find out is not here.
	if strings.Contains(repo, "::") {
		return ""
	}
	if strings.Contains(repo, "://") {
		u, err := url.Parse(repo)
		if err != nil || u.Host == "" {
			return ""
		}
		// Userinfo, query and fragment are all credential-bearing in the wild and
		// none of them belong in a source URL.
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		u.Path = strings.TrimSuffix(u.Path, ".git")
		return u.String()
	}
	// scp-like syntax: [user@]host:path. The user is an ssh login, never a secret,
	// and is dropped because it is not part of the source's identity.
	at := strings.LastIndex(repo, "@")
	colon := strings.Index(repo[at+1:], ":")
	if colon < 0 {
		return ""
	}
	host := repo[at+1 : at+1+colon]
	path := strings.TrimPrefix(repo[at+1+colon+1:], "/")
	if host == "" || path == "" || strings.Contains(host, "/") {
		return ""
	}
	return "https://" + host + "/" + strings.TrimSuffix(path, ".git")
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
	cctx, cancel := context.WithTimeout(ctx, gitRevParseTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-C", dir, "rev-parse", "HEAD")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	// stderr is captured explicitly rather than left to cmd.Output(): "exit status
	// 128" alone cannot be acted on, and this failure is silent by design (the
	// build still succeeds), so the log line is the ONLY evidence it happened.
	// Safe to log — this invocation is given no URL, so unlike the clone there is
	// no credential in anything git can print here.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := clipStderr(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		b.appendLine("[git] rev-parse HEAD failed, omitting the revision label: " + msg)
		return ""
	}
	return strings.TrimSpace(string(out))
}

// clipStderr bounds one captured stderr to what a build log should carry. Split
// out because the boundary is only reachable with an exact-length input, and a
// git message long enough to reach it cannot be produced on demand — so through
// gitRevision alone the cap could be widened, or its comparison flipped, with the
// suite green. Both were verified to survive before this existed.
func clipStderr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLoggedStderr {
		return s
	}
	// Cut on a rune boundary. A byte slice can split a multi-byte git message
	// mid-rune and put invalid UTF-8 into the ring buffer that the job-log API
	// serves; json.Marshal then silently substitutes U+FFFD.
	cut := maxLoggedStderr
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
