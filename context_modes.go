package main

import (
	"io/fs"
	"os"
	"path/filepath"
)

// Build-context permission normalisation.
//
// WHY THIS EXISTS: the modes a build context carries on disk are baked into any
// image whose Dockerfile copies it. A stage that does `COPY <context> /` CREATES
// the destination directories with the SOURCE's mode — and BuildKit applies that
// mode even to a path that already exists in the base image. So a context whose
// files are not world-readable produces an image whose files are not
// world-readable.
//
// That is invisible while everything in the image runs as root, and fatal the
// moment anything drops privileges: a non-root uid that cannot traverse /usr
// cannot exec /usr/bin/python3, and the process dies with EACCES (exit 126).
//
// 📏 It happened. frigate 0.19 — upstream's privilege-drop release — crash-looped
// on kd-nas at ~85 service deaths/minute and 202% CPU with every service dead,
// because /usr, /usr/local, /usr/local/bin and /etc/s6-overlay were all 0770 in
// the built image.
//
// The cause is NOT umask (the agent's is 0022). WORK_DIR lives on a TrueNAS ZFS
// dataset mounted `nfs4acl`, and the work directory carries an INHERITING NFSv4
// ACL (fd---- flags) whose entries cover owner@/group@ and named groups but not
// everyone@. Every file git creates under it inherits that ACL, so its POSIX mode
// renders 0770 whatever the umask says. The tell is that non-executable files
// come out 0770 as well — a umask of 007 would have made them 0660.
//
// Rather than depend on the storage the agent happens to be deployed on, the
// agent normalises the context itself. This is the same `chmod -R a+rX` idiom
// upstream frigate already applies to its own model rootfs.
//
// It is deliberately ADDITIVE: it only ever sets bits, so it cannot change the
// outcome of a build that is already correct. Note that on the ACL storage above
// a+X does add o+x to plain data files, because the ACL has already granted
// owner and group x and a+X keys off "executable by somebody" — that information
// is destroyed before we see it, and a spare x on a config file is harmless.
// Secrets are unaffected because they never live in the context: the vended
// GitHub token is written to the job root, BESIDE the context directory, and
// reaches the build only via `--secret id=ghtoken`.

// normalizeContextModes applies `chmod -R a+rX` semantics to the build context
// rooted at dir: every entry becomes readable by all, and directories plus files
// that are already executable by somebody become executable by all.
//
// Symlinks are skipped rather than followed. os.Chmod resolves a link, so
// chmod'ing one would let a link inside the context (a clone is arbitrary
// third-party content) redirect a mode change onto a file outside it. The
// uploaded-context extractor rejects links outright for the same reason.
func normalizeContextModes(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		want := mode | 0o444 // a+r
		// a+X: directories always, regular files only where an execute bit is
		// already set for somebody — so a file nobody could execute stays
		// non-executable.
		if d.IsDir() || mode&0o111 != 0 {
			want |= 0o111
		}
		if want == mode {
			return nil
		}
		return os.Chmod(path, want)
	})
}
