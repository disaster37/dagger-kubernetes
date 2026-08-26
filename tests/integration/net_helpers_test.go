package integration

import (
	"net"
	"strconv"
	"testing"
)

// freeAddr allocates an OS-assigned free port and returns it as ":<port>".
// Used by integration tests so concurrently/sequentially started servers
// never collide on hardcoded ports (a stale server from a slow shutdown
// could otherwise intercept requests meant for a newer test instance).
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ":" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}
