package handler

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/network"
)

// fakeConn is a minimal network.Conn whose LocalAddr/RemoteAddr are fixed so
// onInternalListener (which matches the local port) can be driven in tests.
type fakeConn struct {
	local  net.Addr
	remote net.Addr
}

func (c *fakeConn) Read([]byte) (int, error)            { return 0, nil }
func (c *fakeConn) Write(b []byte) (int, error)         { return len(b), nil }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) LocalAddr() net.Addr                 { return c.local }
func (c *fakeConn) RemoteAddr() net.Addr                { return c.remote }
func (c *fakeConn) SetDeadline(time.Time) error         { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error     { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error    { return nil }
func (c *fakeConn) Peek(int) ([]byte, error)            { return nil, nil }
func (c *fakeConn) Next(int) ([]byte, error)            { return nil, nil }
func (c *fakeConn) Skip(int) error                      { return nil }
func (c *fakeConn) Release() error                      { return nil }
func (c *fakeConn) Len() int                            { return 0 }
func (c *fakeConn) ReadByte() (byte, error)             { return 0, nil }
func (c *fakeConn) ReadBinary(int) ([]byte, error)      { return nil, nil }
func (c *fakeConn) Malloc(int) ([]byte, error)          { return nil, nil }
func (c *fakeConn) WriteBinary(b []byte) (int, error)   { return len(b), nil }
func (c *fakeConn) Flush() error                        { return nil }
func (c *fakeConn) SetReadTimeout(time.Duration) error  { return nil }
func (c *fakeConn) SetWriteTimeout(time.Duration) error { return nil }

var _ network.Conn = (*fakeConn)(nil)

// addr is a trivial net.Addr.
type addr string

func (a addr) Network() string { return "tcp" }
func (a addr) String() string  { return string(a) }

// newHostCtx builds a RequestContext carrying the given Host and connection
// local port.
func newHostCtx(host string, localPort int, remoteAddr string) *app.RequestContext {
	c := app.NewContext(0)
	c.Request.Header.SetHost(host)
	c.SetConn(&fakeConn{
		local:  addr(net.JoinHostPort("10.0.0.1", strconv.Itoa(localPort))),
		remote: addr(remoteAddr),
	})
	return c
}

// internalServer builds a Server configured with an internal control URL/data
// host and an internal listener port, without starting listeners.
func internalServer(internalControlURL, internalDataHost string, internalPort int) *Server {
	s := &Server{
		cfg: &ServerConfig{
			DataHost:           "data.example.com",
			InternalControlURL: internalControlURL,
			InternalDataHost:   internalDataHost,
		},
		internalControlPort: internalPort,
	}
	return s
}

func TestHostOnly(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://dagger-control.example.svc:80", "dagger-control.example.svc"},
		{"dagger-control.example.svc:80", "dagger-control.example.svc"},
		{"DAGGER-CONTROL.SVC", "dagger-control.svc"},
		{"dagger.example.com", "dagger.example.com"},
		{"[::1]:8080", "::1"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := hostOnly(tc.in); got != tc.want {
			t.Errorf("hostOnly(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestInternalHostForms(t *testing.T) {
	got := internalHostForms("rel-control.ns.svc.cluster.local")
	want := map[string]bool{
		"rel-control.ns.svc.cluster.local": true,
		"rel-control.ns.svc":               true,
		"rel-control":                      true,
	}
	for _, f := range got {
		if !want[f] {
			t.Errorf("unexpected form %q in %v", f, got)
		}
		delete(want, f)
	}
	if len(want) != 0 {
		t.Errorf("missing forms %v from %v", want, got)
	}
}

func TestIsInClusterRequestHostBased(t *testing.T) {
	const internalURL = "https://rel-control.ns.svc:80"
	s := internalServer(internalURL, "rel-data.ns.svc:443", 9000)

	cases := []struct {
		name       string
		host       string
		localPort  int
		fwdHost    string
		remoteAddr string
		want       bool
	}{
		{
			// RFC1918 ingress/LAN remote IP with a public Host → public data host.
			name: "public host from private remote", host: "dagger.example.com",
			localPort: 8080, remoteAddr: "10.42.0.9:5555", want: false,
		},
		{
			name: "internal service host", host: "rel-control.ns.svc:80",
			localPort: 8080, remoteAddr: "10.42.0.9:5555", want: true,
		},
		{
			name: "internal short host", host: "rel-control:80",
			localPort: 8080, remoteAddr: "10.42.0.9:5555", want: true,
		},
		{
			// Follower→leader hop: Host rewritten to the leader pod address,
			// original public Host preserved in the forwarded-host header.
			name: "forwarded public host", host: "leader-pod.ns.svc.cluster.local:8082",
			localPort: 9000, fwdHost: "dagger.example.com", remoteAddr: "10.42.0.9:5555", want: false,
		},
		{
			name: "forwarded internal host", host: "leader-pod.ns.svc.cluster.local:8082",
			localPort: 9000, fwdHost: "rel-control.ns.svc:80", remoteAddr: "10.42.0.9:5555", want: true,
		},
		{
			// Header is NOT honored off the internal listener (spoof-proof).
			name: "forwarded header ignored on public listener", host: "dagger.example.com",
			localPort: 8080, fwdHost: "rel-control.ns.svc:80", remoteAddr: "10.42.0.9:5555", want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newHostCtx(tc.host, tc.localPort, tc.remoteAddr)
			if tc.fwdHost != "" {
				c.Request.Header.Set(forwardedHostHeader, tc.fwdHost)
			}
			if got := s.isInClusterRequest(c); got != tc.want {
				t.Fatalf("isInClusterRequest = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEffectiveHost(t *testing.T) {
	s := internalServer("https://rel-control.ns.svc:80", "rel-data.ns.svc:443", 9000)

	// Off the internal listener, the live Host wins even with a forwarded header.
	c := newHostCtx("dagger.example.com:443", 8080, "10.0.0.9:1")
	c.Request.Header.Set(forwardedHostHeader, "evil.example.com")
	if got := s.effectiveHost(c); got != "dagger.example.com:443" {
		t.Fatalf("effectiveHost (public) = %q, want dagger.example.com:443", got)
	}

	// On the internal listener, the forwarded header is honored.
	c = newHostCtx("leader-pod.ns.svc:8082", 9000, "10.0.0.9:1")
	c.Request.Header.Set(forwardedHostHeader, "rel-control.ns.svc:80")
	if got := s.effectiveHost(c); got != "rel-control.ns.svc:80" {
		t.Fatalf("effectiveHost (internal) = %q, want rel-control.ns.svc:80", got)
	}
}
