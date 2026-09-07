package repository

import (
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/raft"
)

// retryingStreamLayer wraps a tlsStreamLayer and adds DNS re-resolution with
// retry on dial failures. When a StatefulSet pod restarts and gets a new IP,
// eBPF-based DNS proxies (Cilium) may cache the old IP for up to 30 s. This
// layer resolves the hostname to an IP before each dial attempt and retries
// on connection-refused errors, which is the typical symptom of a stale DNS
// cache (the old IP has been reassigned to a pod that is not listening on
// the raft port).
type retryingStreamLayer struct {
	inner    *tlsStreamLayer
	tlsCfg   *tls.Config
	dialer   *net.Dialer
}

var _ raft.StreamLayer = (*retryingStreamLayer)(nil)

// newRetryingStreamLayer wraps inner with DNS-re-resolving dial retry logic.
// The inner layer must be a *tlsStreamLayer (plaintext transports use
// raft.NewTCPTransport directly and do not need this wrapper).
func newRetryingStreamLayer(inner *tlsStreamLayer, timeout time.Duration) *retryingStreamLayer {
	return &retryingStreamLayer{
		inner:  inner,
		tlsCfg: inner.config,
		dialer: &net.Dialer{Timeout: timeout},
	}
}

// maxDialRetries is the maximum number of dial retries when the initial
// attempt fails with a connection-refused error.
const maxDialRetries = 4

// dialRetryBackoff is the base backoff between dial retries.
const dialRetryBackoff = 500 * time.Millisecond

// Dial resolves the hostname in addr to an IP, then dials the IP directly
// with TLS. On connection-refused errors it re-resolves the hostname and
// retries up to maxDialRetries times. Other errors (timeout, TLS handshake
// failure, etc.) are returned immediately.
func (l *retryingStreamLayer) Dial(addr raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	addrStr := string(addr)
	host, port, err := net.SplitHostPort(addrStr)
	if err != nil {
		return nil, fmt.Errorf("parse raft dial addr %s: %w", addrStr, err)
	}

	dialer := l.dialer
	if timeout != dialer.Timeout {
		dialer = &net.Dialer{Timeout: timeout}
	}

	var lastErr error
	for attempt := 0; attempt <= maxDialRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(dialRetryBackoff * time.Duration(attempt))
		}

		// Resolve the hostname to an IP on every attempt so stale DNS
		// caches (Cilium eBPF, NodeLocal DNSCache) are bypassed when
		// the pod IP has changed.
		ips, err := net.LookupHost(host)
		if err != nil {
			lastErr = fmt.Errorf("resolve %s: %w", host, err)
			continue
		}
		if len(ips) == 0 {
			lastErr = fmt.Errorf("resolve %s: no addresses", host)
			continue
		}

		// Dial the resolved IP directly. Set ServerName to the original
		// hostname so the TLS handshake verifies the peer's certificate
		// SANs against the DNS name (not the IP).
		target := net.JoinHostPort(ips[0], port)
		cfg := l.tlsCfg.Clone()
		cfg.ServerName = host

		conn, err := tls.DialWithDialer(dialer, "tcp", target, cfg)
		if err == nil {
			return conn, nil
		}
		lastErr = err

		// Only retry on connection-refused: the peer pod is alive but
		// we got the wrong IP from a stale DNS cache. Timeouts and TLS
		// errors indicate a different problem.
		if !isConnRefused(err) {
			return nil, err
		}
	}

	return nil, fmt.Errorf("dial %s: %w (retries exhausted)", addrStr, lastErr)
}

// isConnRefused reports whether err indicates the remote host actively
// refused the TCP connection (ECONNREFUSED).
func isConnRefused(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "connection refused")
}

func (l *retryingStreamLayer) Accept() (net.Conn, error) {
	return l.inner.Accept()
}

func (l *retryingStreamLayer) Close() error {
	return l.inner.Close()
}

func (l *retryingStreamLayer) Addr() net.Addr {
	return l.inner.Addr()
}
