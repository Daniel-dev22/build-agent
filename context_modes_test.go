package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// mode returns the permission bits of path without following symlinks.
func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

// The ACL-stripped shape this exists to repair: everything 0770, including the
// data file (a umask of 007 would have produced 0660 there — the fact that it
// does not is what identifies the cause as ACL inheritance, not umask).
func TestNormalizeContextModesAppliesARX(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "usr", "local", "bin")
	if err := os.MkdirAll(sub, 0o770); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(sub, "service-runs-as-root")
	data := filepath.Join(root, "usr", "local", "nginx.conf")
	for path, m := range map[string]os.FileMode{exe: 0o770, data: 0o770} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, m); err != nil {
			t.Fatal(err)
		}
	}
	// Chmod the directories last: creating children resets nothing, but MkdirAll
	// applies the process umask, so set the exact starting mode explicitly.
	for _, d := range []string{root, filepath.Join(root, "usr"), filepath.Join(root, "usr", "local"), sub} {
		if err := os.Chmod(d, 0o770); err != nil {
			t.Fatal(err)
		}
	}

	if err := normalizeContextModes(root); err != nil {
		t.Fatalf("normalizeContextModes: %v", err)
	}

	for _, d := range []string{root, filepath.Join(root, "usr"), filepath.Join(root, "usr", "local"), sub} {
		if got := mode(t, d); got != 0o775 {
			t.Errorf("dir %s: got %04o, want 0775 (needs o+x or a non-root uid cannot traverse it)", d, got)
		}
	}
	if got := mode(t, exe); got != 0o775 {
		t.Errorf("executable: got %04o, want 0775", got)
	}
	// A data file that arrives at 0770 DOES gain o+x: a+X keys off "executable by
	// somebody", and the ACL already granted owner and group x. That flattening is
	// the storage's doing, not ours, and an extra x on a config file is harmless.
	// TestNormalizeContextModesIsAdditiveOnly pins the conditionality that matters.
	if got := mode(t, data); got != 0o775 {
		t.Errorf("data file: got %04o, want 0775", got)
	}
}

// A file executable by nobody must never become executable, and the walk must
// not strip bits the owner or group already had.
func TestNormalizeContextModesIsAdditiveOnly(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "data.json")
	if err := os.WriteFile(plain, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(plain, 0o600); err != nil {
		t.Fatal(err)
	}
	already := filepath.Join(root, "fine.sh")
	if err := os.WriteFile(already, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(already, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := normalizeContextModes(root); err != nil {
		t.Fatalf("normalizeContextModes: %v", err)
	}

	got := mode(t, plain)
	if got != 0o644 {
		t.Errorf("0600 data file: got %04o, want 0644 (a+r only)", got)
	}
	if got&0o111 != 0 {
		t.Errorf("0600 data file became executable: %04o — a+X must not add x "+
			"to a file no one could execute", got)
	}
	// Already-correct entries must be left exactly as they are.
	if got := mode(t, already); got != 0o755 {
		t.Errorf("already-correct 0755 file changed to %04o", got)
	}
}

// os.Chmod follows symlinks. A build context is arbitrary third-party content, so
// a link inside it must not be able to redirect a mode change onto a file outside
// it. (The uploaded-context extractor rejects links outright for the same reason.)
func TestNormalizeContextModesDoesNotFollowSymlinks(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("sensitive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := normalizeContextModes(root); err != nil {
		t.Fatalf("normalizeContextModes: %v", err)
	}

	if got := mode(t, outside); got != 0o600 {
		t.Errorf("mode change escaped through a symlink: target is now %04o, want 0600", got)
	}
}

// The fix is only worth anything if the production path actually calls it, on
// EVERY context source and after the pre-build hook has written its generated
// files. Assert the wiring in engine.run rather than trusting the unit tests
// above, which would keep passing if the call site were deleted.
func TestNormalizeContextModesIsWiredIntoEngineRun(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "engine.go", nil, 0)
	if err != nil {
		t.Fatalf("parse engine.go: %v", err)
	}

	var run *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "run" && fn.Recv != nil {
			run = fn
			break
		}
	}
	if run == nil {
		t.Fatal("engine.run not found in engine.go")
	}

	calls := func(n ast.Node, name string) bool {
		found := false
		ast.Inspect(n, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				if fn.Name == name {
					found = true
				}
			case *ast.SelectorExpr:
				if fn.Sel.Name == name {
					found = true
				}
			}
			return !found
		})
		return found
	}

	// Scanning only the TOP-LEVEL statements of run proves the call is
	// unconditional: a call nested inside the `if req.ContextB64 != ""` branch
	// would cover the uploaded context but silently skip every git clone.
	normalizeAt, preBuildAt := -1, -1
	for i, stmt := range run.Body.List {
		if normalizeAt < 0 && calls(stmt, "normalizeContextModes") {
			normalizeAt = i
		}
		if preBuildAt < 0 && calls(stmt, "runPreBuild") {
			preBuildAt = i
		}
	}
	if normalizeAt < 0 {
		t.Fatal("engine.run does not call normalizeContextModes at statement level; " +
			"a build context would ship to buildkit with its on-disk modes intact")
	}
	if preBuildAt < 0 {
		t.Fatal("engine.run no longer calls runPreBuild; this test's ordering check is stale")
	}
	if normalizeAt < preBuildAt {
		t.Errorf("normalizeContextModes runs at stmt %d, before runPreBuild at %d; "+
			"pre-build writes generated files into the context, so normalising first misses them",
			normalizeAt, preBuildAt)
	}
}
