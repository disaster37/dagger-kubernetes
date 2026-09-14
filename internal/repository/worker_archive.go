package repository

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// digestCountWriter forwards writes to an underlying writer while computing a
// hash and counting the bytes, so TarGzipDir can return the compressed
// stream's digest and size without a second pass.
type digestCountWriter struct {
	w     io.Writer
	hash  hash.Hash
	count int64
}

func (d *digestCountWriter) Write(p []byte) (int, error) {
	n, err := d.w.Write(p)
	_, _ = d.hash.Write(p[:n])
	d.count += int64(n)
	return n, err
}

// TarGzipDir writes a gzip tarball of <baseDir>/<subDir> to w, archiving
// entries under the "<subDir>/" prefix (so extraction into baseDir restores
// the subtree). Returns the sha256 hex digest and byte count of the compressed
// stream (computed while writing). Streaming: O(1) memory.
//
// The output is deterministic for the same (paths, modes, sizes, mtimes):
// access/change times are zeroed so re-reading a directory does not alter the
// atime-derived headers.
func TarGzipDir(ctx context.Context, baseDir, subDir string, w io.Writer) (digest string, size int64, err error) {
	return tarGzipWalk(ctx, baseDir, subDir, w, nil)
}

// TarGzipSubset is like TarGzipDir but excludes entries whose path (relative
// to baseDir/subDir, slash-separated) matches any excludePrefixes entry.
// Matching is path-aware: a prefix "content/blobs" excludes the "content/blobs"
// directory and everything under it, but not a sibling like "content/other".
// Used to create the metadata tarball that skips the content store.
func TarGzipSubset(ctx context.Context, baseDir, subDir string, w io.Writer, excludePrefixes ...string) (digest string, size int64, err error) {
	return tarGzipWalk(ctx, baseDir, subDir, w, excludePrefixes)
}

// excludedByPrefix reports whether rel (a slash-separated path relative to
// the tar subDir) matches any exclude prefix.
func excludedByPrefix(rel string, prefixes []string) bool {
	for _, p := range prefixes {
		p = strings.TrimSuffix(p, "/")
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// tarGzipWalker carries the shared walker state (see tarGzipWalk).
type tarGzipWalker struct {
	ctx             context.Context
	tw              *tar.Writer
	subDir          string
	srcDir          string
	excludePrefixes []string
	pruneEmptyDirs  bool

	// pendingDirs buffers not-yet-written directory headers so TarGzipSubset
	// can drop dirs whose subtree is fully excluded. TarGzipDir (no excludes)
	// flushes immediately, preserving its exact historical output. A pending
	// dir is flushed only when a descendant entry materializes; the rest are
	// dropped at the end of the walk.
	pendingDirs []tarPendingDir
	wroteAny    bool
}

// tarPendingDir is one buffered directory header awaiting materialization.
type tarPendingDir struct {
	hdr *tar.Header
	rel string // slash path relative to srcDir ("." for the root)
}

// tarGzipWalk is the shared tar+gzip walker behind TarGzipDir and
// TarGzipSubset: it archives baseDir/subDir, skipping entries excluded by any
// prefix, and returns the compressed stream's sha256 digest and byte count.
// TarGzipSubset additionally prunes directory entries whose entire subtree is
// excluded (keeping the root entry so extraction always recreates the
// top-level dir).
func tarGzipWalk(ctx context.Context, baseDir, subDir string, w io.Writer, excludePrefixes []string) (digest string, size int64, err error) {
	srcDir := filepath.Join(baseDir, subDir)
	if _, err := os.Stat(srcDir); err != nil {
		return "", 0, fmt.Errorf("stat %s: %w", srcDir, err)
	}

	dcw := &digestCountWriter{w: w, hash: sha256.New()}
	gz := gzip.NewWriter(dcw)

	walker := &tarGzipWalker{
		ctx:             ctx,
		tw:              tar.NewWriter(gz),
		subDir:          subDir,
		srcDir:          srcDir,
		excludePrefixes: excludePrefixes,
		pruneEmptyDirs:  len(excludePrefixes) > 0,
	}

	if walkErr := filepath.WalkDir(srcDir, walker.walkEntry); walkErr != nil {
		return "", 0, fmt.Errorf("walk %s: %w", srcDir, walkErr)
	}
	// A fully-excluded subset still emits the root dir entry so extraction
	// recreates the top-level directory.
	if !walker.wroteAny && len(walker.pendingDirs) > 0 {
		if err := walker.tw.WriteHeader(walker.pendingDirs[0].hdr); err != nil {
			return "", 0, err
		}
		walker.pendingDirs = nil
	}
	if err := walker.tw.Close(); err != nil {
		return "", 0, fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return "", 0, fmt.Errorf("close gzip: %w", err)
	}

	return "sha256:" + hex.EncodeToString(dcw.hash.Sum(nil)), dcw.count, nil
}

// walkEntry processes one walked entry (the filepath.WalkDir callback).
func (w *tarGzipWalker) walkEntry(path string, d os.DirEntry, err error) error {
	if err != nil {
		return err
	}
	if ctxErr := w.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	rel, err := filepath.Rel(w.srcDir, path)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	if excludedByPrefix(rel, w.excludePrefixes) {
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	}
	hdr, err := tarEntryHeader(d, path, w.subDir, rel)
	if err != nil {
		return err
	}
	if d.IsDir() {
		return w.writeDir(hdr, rel)
	}
	return w.writeFile(hdr, rel, path, d.Type().IsRegular())
}

// writeDir writes a directory header, buffering it when empty-dir pruning is
// active.
func (w *tarGzipWalker) writeDir(hdr *tar.Header, rel string) error {
	if !w.pruneEmptyDirs {
		return w.tw.WriteHeader(hdr)
	}
	w.pendingDirs = append(w.pendingDirs, tarPendingDir{hdr: hdr, rel: rel})
	return nil
}

// writeFile writes one non-directory entry header (materializing its buffered
// ancestor dirs) and streams the file content when regular.
func (w *tarGzipWalker) writeFile(hdr *tar.Header, rel, path string, regular bool) error {
	if w.pruneEmptyDirs {
		if err := w.flushAncestors(rel); err != nil {
			return err
		}
	}
	if err := w.tw.WriteHeader(hdr); err != nil {
		return err
	}
	w.wroteAny = true
	if !regular {
		return nil
	}
	// The worker dir is engine-owned and trusted: follow symlinks as-is
	// (including outside the tree) so the snapshot captures the dir exactly;
	// os.Root would error on outside-pointing links.
	f, err := os.Open(path) // #nosec G304 -- path comes from WalkDir under srcDir.
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(w.tw, f)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// flushAncestors writes the buffered dir headers that are ancestors of rel.
func (w *tarGzipWalker) flushAncestors(rel string) error {
	kept := w.pendingDirs[:0]
	for _, pd := range w.pendingDirs {
		isAncestor := pd.rel == "." || strings.HasPrefix(rel, pd.rel+"/")
		if isAncestor {
			if err := w.tw.WriteHeader(pd.hdr); err != nil {
				w.pendingDirs = nil
				return err
			}
			w.wroteAny = true
		} else {
			kept = append(kept, pd)
		}
	}
	w.pendingDirs = kept
	return nil
}

// tarEntryHeader builds the tar header for one walked entry, zeroing the
// access/change times (deterministic output) and resolving symlink targets.
func tarEntryHeader(d os.DirEntry, path, subDir, rel string) (*tar.Header, error) {
	info, err := d.Info()
	if err != nil {
		return nil, err
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return nil, err
	}
	hdr.Name = filepath.ToSlash(filepath.Join(subDir, rel))
	if info.IsDir() {
		hdr.Name += "/"
	}
	hdr.AccessTime = hdr.ModTime
	hdr.ChangeTime = hdr.ModTime
	if info.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(path)
		if err != nil {
			return nil, err
		}
		hdr.Linkname = link
	}
	return hdr, nil
}

// UntarGzipDir extracts a gzip tarball produced by TarGzipDir into dstDir,
// streaming from r. Safe against path traversal (rejects absolute and "../"
// entries). Directory entries are created with the archived mode; regular
// files inherit the archived permission bits.
func UntarGzipDir(ctx context.Context, dstDir string, r io.Reader) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		if err := untarEntry(dstDir, hdr, tr); err != nil {
			return err
		}
	}
}

// untarEntry extracts one tar header. Only the entry types TarGzipDir produces
// (dirs, regular files, symlinks) are extracted; anything else is skipped.
func untarEntry(dstDir string, hdr *tar.Header, r io.Reader) error {
	name := filepath.Clean(hdr.Name)
	if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
		return fmt.Errorf("tar entry %q: absolute and ../ paths are not supported", hdr.Name)
	}
	target := filepath.Join(dstDir, name)

	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, hdr.FileInfo().Mode()); err != nil {
			return fmt.Errorf("mkdir %s: %w", target, err)
		}
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(target), err)
		}
		if err := writeTarFile(target, hdr.FileInfo().Mode(), r); err != nil {
			return err
		}
	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(target), err)
		}
		_ = os.Remove(target) // replace any existing entry
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return fmt.Errorf("symlink %s: %w", target, err)
		}
	}
	return nil
}

// writeTarFile streams one regular-file entry to disk.
func writeTarFile(target string, mode os.FileMode, r io.Reader) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode) // #nosec G304 -- target is derived from the validated tar entry.
	if err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", target, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", target, err)
	}
	return nil
}
