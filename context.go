package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Inline build-context upload (BuildRequest.ContextB64).
//
// Some projects' Dockerfile + context don't live in a standalone cloneable repo
// (genmon's are in the ansible repo's genmon/image/ dir). Rather than clone an
// unrelated repo, the caller uploads the context as base64(gzip(tar)) and the
// agent extracts it into the per-job workdir instead of cloning.
//
// SECURITY: the agent's /v1/builds endpoint is cluster-internal (reached only
// through the router's mTLS proxy by an operator with kubectl access) and the
// agent already runs arbitrary Dockerfiles with docker-socket access, so the
// upload adds no new code-exec surface beyond the clone path. The ONE new
// surface is untarring attacker/operator-supplied bytes, so the extractor is
// hardened against the classic archive attacks:
//   - path traversal / zip-slip: every entry must resolve inside destDir.
//   - symlink/hardlink escape: links are rejected outright (a build context has
//     no need for them, and validating link targets safely is error-prone).
//   - special files: only regular files + dirs are written.
//   - decompression bombs: total uncompressed bytes and entry count are capped.
const (
	maxContextBytes   = 64 << 20 // 64 MiB uncompressed — contexts are tiny (genmon ~5 KiB)
	maxContextEntries = 10000
)

// extractContextTarGz decodes base64(gzip(tar)) and writes it into destDir,
// which must already exist. Returns an error (and leaves a partial tree for the
// caller's deferred RemoveAll to clean up) on any malformed or unsafe entry.
func extractContextTarGz(b64, destDir string) error {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return fmt.Errorf("base64 decode: %w", err)
	}
	gz, err := gzip.NewReader(strings.NewReader(string(raw)))
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	// Resolve destDir to an absolute, symlink-free base we can prefix-check against.
	base, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}

	tr := tar.NewReader(io.LimitReader(gz, maxContextBytes+1))
	var (
		written int64
		entries int
	)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		entries++
		if entries > maxContextEntries {
			return fmt.Errorf("context exceeds %d entries", maxContextEntries)
		}

		// Reject absolute paths and any entry that escapes destDir.
		clean := filepath.Clean(hdr.Name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe path in context: %q", hdr.Name)
		}
		target := filepath.Join(base, clean)
		if target != base && !strings.HasPrefix(target, base+string(os.PathSeparator)) {
			return fmt.Errorf("path escapes context root: %q", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			// Don't trust hdr.Size; cap on bytes actually copied. Read one byte
			// past the remaining budget so an oversized entry is detected without
			// streaming the whole (potentially bomb-sized) payload.
			remaining := maxContextBytes - written
			n, err := io.Copy(f, io.LimitReader(tr, remaining+1))
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			written += n
			if written > maxContextBytes {
				return fmt.Errorf("context exceeds %d bytes uncompressed", maxContextBytes)
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("links are not allowed in an uploaded context: %q", hdr.Name)
		default:
			// Skip fifos/devices/etc — a build context has no use for them.
			continue
		}
	}
	if entries == 0 {
		return fmt.Errorf("uploaded context is empty")
	}
	return nil
}
