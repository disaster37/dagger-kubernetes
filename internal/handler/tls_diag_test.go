package handler

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"testing"
	"time"
)

func TestTLSVersionName(t *testing.T) {
	tests := []struct {
		name string
		v    uint16
		want string
	}{
		{"TLS 1.2", tls.VersionTLS12, "TLS 1.2"},
		{"TLS 1.3", tls.VersionTLS13, "TLS 1.3"},
		{"unknown 0x0301", 0x0301, "0x0301"},
		{"unknown 0xffff", 0xffff, "0xffff"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tlsVersionName(tc.v); got != tc.want {
				t.Fatalf("tlsVersionName(0x%04x) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}

func TestClientCN(t *testing.T) {
	tests := []struct {
		name   string
		state  *tls.ConnectionState
		wantCN string
	}{
		{
			name:   "empty state",
			state:  &tls.ConnectionState{},
			wantCN: "",
		},
		{
			name: "no peer certificates",
			state: &tls.ConnectionState{
				PeerCertificates: nil,
			},
			wantCN: "",
		},
		{
			name: "peer with CN",
			state: &tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{{
					Subject: pkix.Name{CommonName: "cn-test"},
				}},
			},
			wantCN: "cn-test",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := clientCN(tc.state); got != tc.wantCN {
				t.Fatalf("clientCN() = %q, want %q", got, tc.wantCN)
			}
		})
	}
}

func TestCertSHA256(t *testing.T) {
	der := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x11, 0x22, 0x33}
	expected := sha256.Sum256(der)
	got := certSHA256(der)
	if len(got) != 32 {
		t.Fatalf("certSHA256 returned %d bytes, want 32", len(got))
	}
	for i := range expected {
		if got[i] != expected[i] {
			t.Fatalf("certSHA256 mismatch at byte %d: got 0x%02x, want 0x%02x", i, got[i], expected[i])
		}
	}
	// Also verify hex encoding matches
	hexStr := hex.EncodeToString(certSHA256(der))
	expectedHex := hex.EncodeToString(expected[:])
	if hexStr != expectedHex {
		t.Fatalf("hex.EncodeToString(certSHA256(der)) = %q, want %q", hexStr, expectedHex)
	}
}

func TestLogDataPlaneServingCert(t *testing.T) {
	env := newTestEnv(t)
	s := env.server

	// Case (a): empty certificate — must not panic.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("logDataPlaneServingCert(empty) panicked: %v", r)
			}
		}()
		var emptyCert tls.Certificate
		s.logDataPlaneServingCert(&emptyCert)
	}()

	// Case (b): valid self-signed cert — must not panic and should log info.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("logDataPlaneServingCert(valid) panicked: %v", r)
			}
		}()
		priv, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		template := &x509.Certificate{
			SerialNumber: big.NewInt(1),
			Subject: pkix.Name{
				CommonName:   "test.example.com",
				Organization: []string{"Test Org"},
			},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(time.Hour),
			KeyUsage:              x509.KeyUsageDigitalSignature,
			BasicConstraintsValid: true,
			DNSNames:              []string{"test.example.com"},
		}
		certDER, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
		if err != nil {
			t.Fatalf("create cert: %v", err)
		}
		validCert := tls.Certificate{
			Certificate: [][]byte{certDER},
			PrivateKey:  priv,
		}
		s.logDataPlaneServingCert(&validCert)
	}()

	// Case (c): unparseable DER — must not panic.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("logDataPlaneServingCert(unparseable) panicked: %v", r)
			}
		}()
		unparseable := tls.Certificate{
			Certificate: [][]byte{{0x01, 0x02}},
		}
		s.logDataPlaneServingCert(&unparseable)
	}()
}
