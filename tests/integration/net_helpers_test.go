package integration

import (
	"net"
	"strconv"
	"testing"
)

// freeListener binds an OS-assigned loopback port and returns the open
// listener. Integration tests hand it to the supervisor via
// ServerConfig.ControlListener/DataListener instead of releasing the port
// first: a probe-then-close helper leaves a TOCTOU window where an httptest
// server (or a concurrent test binary) can claim the port before the server
// binds it, which flaked TestOIDCLoginForbiddenFlow with
// "tcp listen: ... bind: address already in use". A bound listener cannot be
// stolen, so the race is removed rather than retried.
func freeListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate listener: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// listenerAddr formats a listener's port as the ":<port>" ServerConfig
// address. Tests keep passing the address because it is embedded in URLs
// (pipeline links, CLI base URL, OAuth redirect URL) before the server starts.
func listenerAddr(ln net.Listener) string {
	return ":" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}
