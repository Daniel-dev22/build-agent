package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// tarGzB64 builds a base64(gzip(tar)) blob from the given entries for tests.
type tarEntry struct {
	name     string
	typeflag byte
	body     string
	linkname string
}

func tarGzB64(t *testing.T, entries []tarEntry) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Linkname: e.linkname, Mode: 0o644}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if e.typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gz: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestExtractContext_HappyPath(t *testing.T) {
	dir := t.TempDir()
	b64 := tarGzB64(t, []tarEntry{
		{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM ubuntu:latest\n"},
		{name: "sub", typeflag: tar.TypeDir},
		{name: "sub/healthcheck.py", typeflag: tar.TypeReg, body: "print('ok')\n"},
	})
	if err := extractContextTarGz(b64, dir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil || string(got) != "FROM ubuntu:latest\n" {
		t.Fatalf("Dockerfile mismatch: %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "healthcheck.py")); err != nil {
		t.Fatalf("nested file missing: %v", err)
	}
}

func TestExtractContext_RejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"../escape.txt", "../../etc/passwd", "/abs/evil"} {
		b64 := tarGzB64(t, []tarEntry{{name: name, typeflag: tar.TypeReg, body: "x"}})
		if err := extractContextTarGz(b64, dir); err == nil {
			t.Fatalf("expected rejection for %q, got nil", name)
		}
		// The escaping file must not exist outside the dir.
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.txt")); err == nil {
			t.Fatalf("traversal wrote outside destDir for %q", name)
		}
	}
}

func TestExtractContext_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	b64 := tarGzB64(t, []tarEntry{{name: "link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}})
	if err := extractContextTarGz(b64, dir); err == nil {
		t.Fatal("expected symlink rejection, got nil")
	}
}

func TestExtractContext_RejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	b64 := tarGzB64(t, nil)
	if err := extractContextTarGz(b64, dir); err == nil {
		t.Fatal("expected empty-context rejection, got nil")
	}
}

func TestExtractContext_RejectsBadBase64(t *testing.T) {
	if err := extractContextTarGz("!!!not base64!!!", t.TempDir()); err == nil {
		t.Fatal("expected base64 error, got nil")
	}
}
