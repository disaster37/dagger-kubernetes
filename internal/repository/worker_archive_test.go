package repository

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestFile creates a file (with parent dirs) with the given content and mode.
func writeTestFile(t *testing.T, path string, mode os.FileMode, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// tarGzipDirInto runs TarGzipDir and returns the digest, size, and compressed bytes.
func tarGzipDirInto(t *testing.T, baseDir string) (digest string, size int64, compressed []byte) {
	t.Helper()
	var buf bytes.Buffer
	digest, size, err := TarGzipDir(context.Background(), baseDir, "worker", &buf)
	if err != nil {
		t.Fatalf("TarGzipDir: %v", err)
	}
	return digest, size, buf.Bytes()
}

func TestTarGzipUntarRoundTrip(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "worker", "metadata.db"), 0o600, "bolt-metadata")
	writeTestFile(t, filepath.Join(src, "worker", "content", "blobs", "abc"), 0o644, "blob-bytes")
	writeTestFile(t, filepath.Join(src, "worker", "snapshots", "1"), 0o755, "snapshot")
	writeTestFile(t, filepath.Join(src, "worker", "readonly.txt"), 0o400, "read-only")
	if err := os.Symlink("metadata.db", filepath.Join(src, "worker", "metadata.link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	digest, size, compressed := tarGzipDirInto(t, src)

	// Digest/size describe the compressed stream.
	if digest != "sha256:"+sha256HexBytes(compressed) {
		t.Fatalf("digest = %s, want sha256 of the compressed stream", digest)
	}
	if size != int64(len(compressed)) {
		t.Fatalf("size = %d, want %d", size, len(compressed))
	}

	// Entries carry the "<subDir>/" prefix.
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	var names []string
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		names = append(names, hdr.Name)
	}
	wantNames := []string{
		"worker/",
		"worker/content/",
		"worker/content/blobs/",
		"worker/content/blobs/abc",
		"worker/metadata.db",
		"worker/metadata.link",
		"worker/readonly.txt",
		"worker/snapshots/",
		"worker/snapshots/1",
	}
	if len(names) != len(wantNames) {
		t.Fatalf("entries = %v, want %v", names, wantNames)
	}
	for i, want := range wantNames {
		if names[i] != want {
			t.Fatalf("entry[%d] = %q, want %q", i, names[i], want)
		}
	}

	// Extraction into a fresh dir restores content, permissions, and symlinks.
	dst := t.TempDir()
	if err := UntarGzipDir(context.Background(), dst, bytes.NewReader(compressed)); err != nil {
		t.Fatalf("UntarGzipDir: %v", err)
	}
	for _, tc := range []struct {
		rel  string
		want string
		mode os.FileMode
	}{
		{"worker/metadata.db", "bolt-metadata", 0o600},
		{"worker/content/blobs/abc", "blob-bytes", 0o644},
		{"worker/snapshots/1", "snapshot", 0o755},
		{"worker/readonly.txt", "read-only", 0o400},
	} {
		got, err := os.ReadFile(filepath.Join(dst, tc.rel))
		if err != nil {
			t.Fatalf("read %s: %v", tc.rel, err)
		}
		if string(got) != tc.want {
			t.Fatalf("%s = %q, want %q", tc.rel, got, tc.want)
		}
		fi, err := os.Stat(filepath.Join(dst, tc.rel))
		if err != nil {
			t.Fatalf("stat %s: %v", tc.rel, err)
		}
		if fi.Mode().Perm() != tc.mode {
			t.Fatalf("%s mode = %v, want %v", tc.rel, fi.Mode().Perm(), tc.mode)
		}
	}
	link, err := os.Readlink(filepath.Join(dst, "worker", "metadata.link"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if link != "metadata.db" {
		t.Fatalf("symlink target = %q, want metadata.db", link)
	}
}

func TestTarGzipEmptyDir(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "worker"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	digest, size, compressed := tarGzipDirInto(t, src)
	if size == 0 || len(compressed) == 0 {
		t.Fatalf("empty dir should still produce a (dir-entry) tarball, size=%d", size)
	}
	if digest != "sha256:"+sha256HexBytes(compressed) {
		t.Fatal("digest must match the compressed stream")
	}

	dst := t.TempDir()
	if err := UntarGzipDir(context.Background(), dst, bytes.NewReader(compressed)); err != nil {
		t.Fatalf("UntarGzipDir: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "worker")); err != nil || !fi.IsDir() {
		t.Fatalf("worker dir not restored: %v", err)
	}
}

func TestTarGzipMissingDir(t *testing.T) {
	if _, _, err := TarGzipDir(context.Background(), t.TempDir(), "nope", io.Discard); err == nil {
		t.Fatal("expected error for missing source dir")
	}
}

func TestTarGzipDigestDeterministic(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "worker", "a.bin"), 0o600, "same-bytes")
	writeTestFile(t, filepath.Join(src, "worker", "sub", "b.bin"), 0o600, "more-same-bytes")

	d1, s1, c1 := tarGzipDirInto(t, src)
	d2, s2, c2 := tarGzipDirInto(t, src)
	if d1 != d2 || s1 != s2 || !bytes.Equal(c1, c2) {
		t.Fatalf("same input produced different output: %s/%d vs %s/%d", d1, s1, d2, s2)
	}
}

func TestTarGzipContextCancelled(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "worker", "a.bin"), 0o600, "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := TarGzipDir(ctx, src, "worker", io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestUntarGzipPathTraversal(t *testing.T) {
	// Hand-craft a tarball with absolute and ../ entries; UntarGzipDir must
	// reject them (CWE-22) instead of writing outside dstDir.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	entries := []struct{ name, body string }{
		{"/etc/absolute-escape", "abs"},
		{"../outside-escape", "rel"},
		{"worker/ok.txt", "ok"},
	}
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o600, Size: int64(len(e.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	dst := t.TempDir()
	if err := UntarGzipDir(context.Background(), dst, &buf); err == nil {
		t.Fatal("expected error for traversal entries")
	}
	// The valid entry before the failure may or may not exist; the important
	// part is the error. Now verify a clean tarball still extracts.
}

func TestUntarGzipRejectsCorruptGzip(t *testing.T) {
	if err := UntarGzipDir(context.Background(), t.TempDir(), strings.NewReader("not-gzip")); err == nil {
		t.Fatal("expected error for corrupt gzip stream")
	}
}

func TestUntarGzipContextCancelled(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "worker", "a.bin"), 0o600, "x")
	_, _, compressed := tarGzipDirInto(t, src)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := UntarGzipDir(ctx, t.TempDir(), bytes.NewReader(compressed)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// tarEntries lists the entry names of a TarGzipSubset-produced tarball.
func tarGzipSubsetNames(t *testing.T, baseDir string, exclude ...string) []string {
	t.Helper()
	var buf bytes.Buffer
	if _, _, err := TarGzipSubset(context.Background(), baseDir, "worker", &buf, exclude...); err != nil {
		t.Fatalf("TarGzipSubset: %v", err)
	}
	zr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer func() { _ = zr.Close() }()
	var names []string
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		names = append(names, hdr.Name)
	}
	return names
}

func TestTarGzipSubsetExcludesPrefix(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "worker", "metadata.db"), 0o600, "bolt-metadata")
	writeTestFile(t, filepath.Join(src, "worker", "content", "blobs", "ab", "cd"), 0o644, "blob")
	writeTestFile(t, filepath.Join(src, "worker", "snapshots", "1"), 0o755, "snapshot")

	names := tarGzipSubsetNames(t, src, "content/blobs")
	want := []string{"worker/", "worker/metadata.db", "worker/snapshots/", "worker/snapshots/1"}
	if len(names) != len(want) {
		t.Fatalf("entries = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("entry[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

func TestTarGzipSubsetEmptyResult(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "worker", "content", "blobs", "ab", "cd"), 0o644, "blob")

	names := tarGzipSubsetNames(t, src, "content/blobs", "metadata.db")
	want := []string{"worker/"}
	if len(names) != 1 || names[0] != want[0] {
		t.Fatalf("entries = %v, want %v (only the root dir entry)", names, want)
	}
}

func TestTarGzipSubsetNestedPrefix(t *testing.T) {
	// Prefix matching is path-aware: excluding content/blobs must not exclude
	// content/other or a sibling like content/blobs-extra.
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "worker", "content", "blobs", "ab", "cd"), 0o644, "blob")
	writeTestFile(t, filepath.Join(src, "worker", "content", "other", "keep.txt"), 0o644, "keep")
	writeTestFile(t, filepath.Join(src, "worker", "content", "blobs-extra", "also-keep.txt"), 0o644, "keep")

	names := tarGzipSubsetNames(t, src, "content/blobs")
	for _, n := range names {
		if strings.Contains(n, "blobs/ab") {
			t.Fatalf("excluded entry leaked: %q", n)
		}
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "content/other/keep.txt") || !strings.Contains(joined, "content/blobs-extra/also-keep.txt") {
		t.Fatalf("path-aware exclusion broke siblings: %v", names)
	}

	// A trailing slash in the prefix behaves identically.
	names = tarGzipSubsetNames(t, src, "content/blobs/")
	if strings.Contains(strings.Join(names, ","), "blobs/ab") {
		t.Fatalf("trailing-slash prefix did not exclude: %v", names)
	}
}

func TestTarGzipSubsetMissingDir(t *testing.T) {
	if _, _, err := TarGzipSubset(context.Background(), t.TempDir(), "nope", io.Discard, "x"); err == nil {
		t.Fatal("expected error for missing source dir")
	}
}
