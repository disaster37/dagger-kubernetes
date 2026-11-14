package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestAuthUIRegressionHarness runs the Node stdlib auth regression harness. It
// transpiles and exercises the real auth-navigation/interceptor modules; the
// test skips ONLY when Node or the installed UI dependencies are genuinely
// absent, and any harness assertion/compile failure fails the test.
func TestAuthUIRegressionHarness(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not available; skipping UI auth harness: %v", err)
	}
	uiDir := uiDir(t)
	if _, err := os.Stat(filepath.Join(uiDir, "node_modules", "typescript")); err != nil {
		t.Skipf("UI dependencies not installed; run npm --prefix ui ci first: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--experimental-vm-modules", "tests/auth-regression.mjs")
	cmd.Dir = uiDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("auth regression harness failed: %v\n%s", err, out)
	}
	t.Logf("auth regression harness output:\n%s", out)
}

// uiDir locates the repository's ui/ directory relative to this test file.
func uiDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dir := filepath.Join(root, "ui")
	if _, err := os.Stat(filepath.Join(dir, "tests", "auth-regression.mjs")); err != nil {
		t.Fatalf("auth harness not found under %s: %v", dir, err)
	}
	return dir
}
