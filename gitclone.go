package main

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"regexp"
)

// Git clone for every repo the agent fetches: the app repo and each named build context.
//
// The vended token is handed to git as an http.extraheader through the GIT_CONFIG_COUNT env
// vars, NOT embedded in the clone URL. A URL with userinfo is written verbatim by `git clone` into
// <clone>/.git/config as remote.origin.url, and the clone dir is the docker build context — so
// the token would be shipped to BuildKit inside the context, and it would also be visible in
// `ps` argv. With the header form the URL is token-free, so neither .git/config nor argv ever
// carries it, and the build log (which tees git's output) cannot echo it either.

// githubCloneURL is the token-free https clone form of an ssh or https GitHub repo URL.
func githubCloneURL(repo string) string {
	return "https://github.com/" + repoSlug(repo) + ".git"
}

// gitAuthEnv returns the env that authenticates git against github.com with token, via
// http.https://github.com/.extraheader. Needs git >= 2.31 (GIT_CONFIG_COUNT).
func gitAuthEnv(token string) []string {
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: basic " + basic,
	}
}

// gitClone clones repo@ref (shallow) into dir. For private repos a vended GitHub App token
// authenticates the clone through gitAuthEnv; it never appears in the URL, argv or .git/config.
func (e *engine) gitClone(ctx context.Context, b *Build, repo, ref, dir, token string) error {
	cctx, cancel := context.WithTimeout(ctx, e.cfg.GitTimeout)
	defer cancel()
	if shaRe.MatchString(ref) {
		for _, cmd := range shaCmds(cctx, repo, ref, dir, token) {
			if err := e.stream(b, "git", cmd, nil); err != nil {
				return err
			}
		}
		return nil
	}
	return e.stream(b, "git", cloneCmd(cctx, repo, ref, dir, token), nil)
}

// shaRe matches a full commit SHA. `git clone --branch` rejects one, so a ref pinned by SHA is
// fetched directly (GitHub serves any reachable commit).
var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// shaCmds is init + shallow fetch of one commit + detached checkout. The URL is a positional
// argument of fetch, so it is never written to .git/config; auth is the same env header as a clone.
func shaCmds(ctx context.Context, repo, sha, dir, token string) []*exec.Cmd {
	url := repo
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if token != "" {
		url = githubCloneURL(repo)
		env = append(env, gitAuthEnv(token)...)
	}
	mk := func(args ...string) *exec.Cmd {
		c := exec.CommandContext(ctx, "git", args...)
		c.Env = env
		return c
	}
	return []*exec.Cmd{
		mk("init", "-q", "--", dir),
		mk("-C", dir, "fetch", "-q", "--depth", "1", "--", url, sha),
		mk("-C", dir, "checkout", "-q", "--detach", "FETCH_HEAD"),
	}
}

// cloneCmd assembles the clone command. Pure (no exec) so a test can pin that the token reaches
// git through the environment only.
func cloneCmd(ctx context.Context, repo, ref, dir, token string) *exec.Cmd {
	url := repo
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if token != "" {
		url = githubCloneURL(repo)
		env = append(env, gitAuthEnv(token)...)
	}
	cmd := exec.CommandContext(ctx, "git", cloneArgs(url, ref, dir)...)
	cmd.Env = env
	return cmd
}
