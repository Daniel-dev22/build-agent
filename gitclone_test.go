package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testToken = "ghs_SECRETTOKEN0123456789"

func TestCloneCmdKeepsTokenOutOfArgv(t *testing.T) {
	cmd := cloneCmd(context.Background(), "git@github.com:o/r.git", "main", "/tmp/d", testToken)
	for _, a := range cmd.Args {
		if strings.Contains(a, testToken) {
			t.Fatalf("token in argv: %q", a)
		}
	}
	if !strings.Contains(strings.Join(cmd.Args, " "), "https://github.com/o/r.git") {
		t.Fatalf("clone url not the token-free https form: %v", cmd.Args)
	}
	var header string
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "GIT_CONFIG_VALUE_0=") {
			header = kv
		}
	}
	if !strings.Contains(header, "Authorization: basic ") {
		t.Fatalf("auth must travel as an http extraheader env value, env=%v", cmd.Env)
	}
	if !strings.Contains(strings.Join(cmd.Env, "\n"), "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader") {
		t.Fatalf("extraheader key missing: %v", cmd.Env)
	}
}

func TestCloneCmdWithoutTokenAddsNoAuth(t *testing.T) {
	cmd := cloneCmd(context.Background(), "https://github.com/o/pub.git", "", "/tmp/d", "")
	if strings.Contains(strings.Join(cmd.Env, "\n"), "GIT_CONFIG_VALUE_0=Authorization") {
		t.Fatal("a public clone must not carry an auth header")
	}
}

// The clone dir is the docker build context. A token that reaches .git/config is shipped to
// BuildKit, so assert on the bytes of every file in the clone, not on the helper's shape.
func TestClonedContextNeverContainsToken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	src := filepath.Join(root, "origin")
	mustGit := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustGit("init", "-q", src)
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit("-C", src, "add", "f")
	mustGit("-C", src, "commit", "-q", "-m", "c")

	// Redirect the github https URL to the local repo so the TOKEN branch runs without a network.
	gcfg := filepath.Join(root, "gitconfig")
	// The second rule also redirects the legacy userinfo-in-URL form, so a regression back to it
	// still clones (and writes the token into .git/config) instead of failing for the wrong reason.
	rules := "[url \"file://" + src + "\"]\n\tinsteadOf = https://github.com/o/r.git\n\tinsteadOf = https://x-access-token:" + testToken + "@github.com/o/r.git\n"
	if err := os.WriteFile(gcfg, []byte(rules), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gcfg)

	dst := filepath.Join(root, "clone")
	e := &engine{cfg: Config{GitTimeout: 30e9}}
	b := &Build{subscribers: map[chan string]struct{}{}}
	if err := e.gitClone(context.Background(), b, "git@github.com:o/r.git", "", dst, testToken); err != nil {
		t.Fatalf("clone: %v\n%s", err, strings.Join(b.logLines(), "\n"))
	}
	found := false
	_ = filepath.WalkDir(dst, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		found = true
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), testToken) {
			t.Errorf("token found in clone file %s", p)
		}
		return nil
	})
	if !found {
		t.Fatal("clone produced no files; test proves nothing")
	}
	if strings.Contains(strings.Join(b.logLines(), "\n"), testToken) {
		t.Error("token echoed into the build log")
	}
}
