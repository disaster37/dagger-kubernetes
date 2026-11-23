package handler

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	rafttransport "github.com/disaster37/go-raft/transport"
)

// writeLeaf writes a leaf's certificate and key as PEM files for the static
// keypair path.
func writeLeaf(t *testing.T, certPath, keyPath string, leaf *tls.Certificate) {
	t.Helper()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Certificate[0]})
	keyDER, err := x509.MarshalPKCS8PrivateKey(leaf.PrivateKey)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

// selfSignedLeaf builds a throwaway self-signed leaf for TLS-routing tests.
func selfSignedLeaf(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		BasicConstraintsValid: true,
		DNSNames:              []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

// TestControlTLSConfigRoutesInternalSNIViaGetConfigForClient locks the fix for
// the GetConfigForClient precedence pitfall: a dynamic provider's
// ServerTLSConfig() sets GetConfigForClient, which Go's TLS stack calls before
// GetCertificate. The control listener must therefore route internal SNIs
// through GetConfigForClient (not only GetCertificate).
func TestControlTLSConfigRoutesInternalSNIViaGetConfigForClient(t *testing.T) {
	publicLeaf := selfSignedLeaf(t, "data.example.com")
	internalLeaf := selfSignedLeaf(t, "rel-control.ns.svc")

	provider := rafttransport.NewDynamicTLSProvider(publicLeaf, nil, nil, false)
	s := internalServer("https://rel-control.ns.svc:80", "rel-data.ns.svc:443", 0)
	s.cfg.ControlTLSProvider = provider
	s.cfg.InternalTLSCert = &internalLeaf

	cfg, err := s.controlTLSConfig()
	if err != nil {
		t.Fatalf("controlTLSConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("controlTLSConfig = nil, want a config")
	}
	if cfg.GetConfigForClient == nil {
		t.Fatal("GetConfigForClient must be wrapped so internal SNIs are routed")
	}

	// (a) Internal SNI: the returned config must carry the internal leaf, not
	// the provider's public config.
	internalCfg, err := cfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "rel-control.ns.svc"})
	if err != nil {
		t.Fatalf("GetConfigForClient(internal): %v", err)
	}
	if internalCfg == nil {
		t.Fatal("internal SNI must yield a per-connection config")
	}
	if len(internalCfg.Certificates) != 1 {
		t.Fatalf("internal config certificates = %d, want 1", len(internalCfg.Certificates))
	}
	if &internalCfg.Certificates[0] == &publicLeaf {
		t.Fatal("internal SNI must not receive the provider's public certificate")
	}
	if internalCfg.GetCertificate != nil {
		t.Fatal("internal config must carry the leaf directly, not the provider's GetCertificate")
	}
	if internalCfg.ClientAuth != tls.NoClientCert {
		t.Fatalf("public control listener must not require client certs, got ClientAuth=%v", internalCfg.ClientAuth)
	}
	// The internal leaf's CN must be the internal host, proving it is the
	// minting-CA internal leaf rather than the public one.
	leaf, err := x509.ParseCertificate(internalCfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse internal leaf: %v", err)
	}
	if leaf.Subject.CommonName != "rel-control.ns.svc" {
		t.Fatalf("internal SNI cert CN = %q, want rel-control.ns.svc", leaf.Subject.CommonName)
	}

	// (b) Public SNI: delegate to the provider's original GetConfigForClient,
	// which returns the provider's GetCertificate (the public cert path).
	publicCfg, err := cfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "data.example.com"})
	if err != nil {
		t.Fatalf("GetConfigForClient(public): %v", err)
	}
	if publicCfg == nil {
		t.Fatal("public SNI must delegate to the provider's GetConfigForClient")
	}
	if publicCfg.GetCertificate == nil {
		t.Fatal("public config must delegate to the provider's GetCertificate")
	}
	got, err := publicCfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "data.example.com"})
	if err != nil {
		t.Fatalf("public GetCertificate: %v", err)
	}
	if got == nil || len(got.Certificate) == 0 {
		t.Fatal("public GetCertificate returned no certificate")
	}
	pubLeaf, err := x509.ParseCertificate(got.Certificate[0])
	if err != nil {
		t.Fatalf("parse public leaf: %v", err)
	}
	if pubLeaf.Subject.CommonName != "data.example.com" {
		t.Fatalf("public SNI cert CN = %q, want data.example.com", pubLeaf.Subject.CommonName)
	}
}

// TestControlTLSConfigStaticKeypairInternalSNI verifies the second-layer
// GetCertificate wrapper still routes internal SNIs when no dynamic provider is
// configured (static keypair path).
func TestControlTLSConfigStaticKeypairInternalSNI(t *testing.T) {
	publicLeaf := selfSignedLeaf(t, "data.example.com")
	internalLeaf := selfSignedLeaf(t, "rel-control.ns.svc")

	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	writeLeaf(t, certPath, keyPath, &publicLeaf)

	s := internalServer("https://rel-control.ns.svc:80", "rel-data.ns.svc:443", 0)
	s.cfg.CertPath = certPath
	s.cfg.KeyPath = keyPath
	s.cfg.InternalTLSCert = &internalLeaf

	cfg, err := s.controlTLSConfig()
	if err != nil {
		t.Fatalf("controlTLSConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("controlTLSConfig = nil, want a config")
	}
	// Static path: the provider's GetConfigForClient is nil, so for a public
	// SNI the wrapper returns (nil, nil) and the handshake falls through to the
	// outer GetCertificate wrapper.
	if cfg.GetConfigForClient != nil {
		if publicCfg, err := cfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "data.example.com"}); err != nil || publicCfg != nil {
			t.Fatalf("static public SNI GetConfigForClient = (%v, %v), want (nil, nil)", publicCfg, err)
		}
	}
	got, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "rel-control.ns.svc"})
	if err != nil {
		t.Fatalf("GetCertificate(internal): %v", err)
	}
	leaf, err := x509.ParseCertificate(got.Certificate[0])
	if err != nil {
		t.Fatalf("parse internal leaf: %v", err)
	}
	if leaf.Subject.CommonName != "rel-control.ns.svc" {
		t.Fatalf("internal SNI cert CN = %q, want rel-control.ns.svc", leaf.Subject.CommonName)
	}
	pub, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "data.example.com"})
	if err != nil {
		t.Fatalf("GetCertificate(public): %v", err)
	}
	pubLeaf, err := x509.ParseCertificate(pub.Certificate[0])
	if err != nil {
		t.Fatalf("parse public leaf: %v", err)
	}
	if pubLeaf.Subject.CommonName != "data.example.com" {
		t.Fatalf("public SNI cert CN = %q, want data.example.com", pubLeaf.Subject.CommonName)
	}
}

// TestControlTLSConfigInternalSNIFallsBackWhenNoInternalCert covers the
// internalCert() failure branch: an internal SNI with no configured internal
// cert must fall back to the public certificate. With a dynamic provider the
// GetConfigForClient wrapper delegates to the provider's original
// GetConfigForClient; with the static keypair it returns (nil, nil) so the
// handshake falls through to the outer GetCertificate wrapper.
func TestControlTLSConfigInternalSNIFallsBackWhenNoInternalCert(t *testing.T) {
	t.Run("dynamic provider delegates to public GetConfigForClient", func(t *testing.T) {
		publicLeaf := selfSignedLeaf(t, "data.example.com")
		provider := rafttransport.NewDynamicTLSProvider(publicLeaf, nil, nil, false)
		s := internalServer("https://rel-control.ns.svc:80", "rel-data.ns.svc:443", 0)
		s.cfg.ControlTLSProvider = provider
		// No InternalTLSCert/InternalTLSProvider: internalCert() fails.
		cfg, err := s.controlTLSConfig()
		if err != nil {
			t.Fatalf("controlTLSConfig: %v", err)
		}
		if cfg == nil || cfg.GetConfigForClient == nil {
			t.Fatal("controlTLSConfig must wrap GetConfigForClient")
		}
		got, err := cfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "rel-control.ns.svc"})
		if err != nil {
			t.Fatalf("GetConfigForClient(internal): %v", err)
		}
		if got == nil {
			t.Fatal("internal SNI must delegate to the provider's GetConfigForClient")
		}
		if got.GetCertificate == nil {
			t.Fatal("delegated config must carry the provider's GetCertificate")
		}
		leaf, err := got.GetCertificate(&tls.ClientHelloInfo{ServerName: "rel-control.ns.svc"})
		if err != nil {
			t.Fatalf("provider GetCertificate: %v", err)
		}
		parsed, err := x509.ParseCertificate(leaf.Certificate[0])
		if err != nil {
			t.Fatalf("parse leaf: %v", err)
		}
		if parsed.Subject.CommonName != "data.example.com" {
			t.Fatalf("fallback cert CN = %q, want the public data.example.com", parsed.Subject.CommonName)
		}
	})

	t.Run("static keypair returns nil and falls through to GetCertificate", func(t *testing.T) {
		publicLeaf := selfSignedLeaf(t, "data.example.com")
		dir := t.TempDir()
		certPath := filepath.Join(dir, "tls.crt")
		keyPath := filepath.Join(dir, "tls.key")
		writeLeaf(t, certPath, keyPath, &publicLeaf)

		s := internalServer("https://rel-control.ns.svc:80", "rel-data.ns.svc:443", 0)
		s.cfg.CertPath = certPath
		s.cfg.KeyPath = keyPath
		// No InternalTLSCert/InternalTLSProvider: internalCert() fails.
		cfg, err := s.controlTLSConfig()
		if err != nil {
			t.Fatalf("controlTLSConfig: %v", err)
		}
		if cfg == nil || cfg.GetConfigForClient == nil {
			t.Fatal("controlTLSConfig must wrap GetConfigForClient")
		}
		got, err := cfg.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "rel-control.ns.svc"})
		if err != nil {
			t.Fatalf("GetConfigForClient(internal): %v", err)
		}
		if got != nil {
			t.Fatal("static path with no internal cert must return (nil, nil) to fall through")
		}
		// The outer GetCertificate wrapper must then serve the public leaf.
		leaf, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "rel-control.ns.svc"})
		if err != nil {
			t.Fatalf("GetCertificate(internal): %v", err)
		}
		parsed, err := x509.ParseCertificate(leaf.Certificate[0])
		if err != nil {
			t.Fatalf("parse leaf: %v", err)
		}
		if parsed.Subject.CommonName != "data.example.com" {
			t.Fatalf("fallback cert CN = %q, want the public data.example.com", parsed.Subject.CommonName)
		}
	})
}

// TestMatchesInternalSNIExactForms verifies that only the EXACT configured
// internal host forms are treated as internal SNIs: an arbitrary ".svc" name
// that is not the configured control/data host must fall back to the public
// certificate, as must the public hostname.
func TestMatchesInternalSNIExactForms(t *testing.T) {
	s := internalServer("https://rel-control.ns.svc:80", "rel-data.ns.svc:443", 0)
	s.cfg.ClusterDomain = "cluster.local"
	cases := []struct {
		name string
		sni  string
		want bool
	}{
		{"control short", "rel-control", true},
		{"control namespaced", "rel-control.ns.svc", true},
		{"control fqdn", "rel-control.ns.svc.cluster.local", true},
		{"data short", "rel-data", true},
		{"data namespaced", "rel-data.ns.svc", true},
		{"data fqdn", "rel-data.ns.svc.cluster.local", true},
		{"arbitrary svc is public", "evil.svc", false},
		{"arbitrary namespaced svc is public", "other.ns.svc", false},
		{"arbitrary fqdn svc is public", "attacker.ns.svc.cluster.local", false},
		{"public hostname", "data.example.com", false},
		{"trailing dot trimmed", "rel-control.ns.svc.", true},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.matchesInternalSNI(tc.sni); got != tc.want {
				t.Errorf("matchesInternalSNI(%q) = %v, want %v", tc.sni, got, tc.want)
			}
		})
	}
}

// TestMatchesInternalSNINoConfiguredHost verifies that with no internal hosts
// configured, every SNI (including ".svc" names) is treated as public.
func TestMatchesInternalSNINoConfiguredHost(t *testing.T) {
	s := internalServer("", "", 0)
	for _, sni := range []string{"svc.ns.svc", "evil.svc", "data.example.com"} {
		if s.matchesInternalSNI(sni) {
			t.Errorf("matchesInternalSNI(%q) = true with no internal host configured", sni)
		}
	}
}

// TestInternalCertPrecedence verifies the cert-selection branch the SNI routing
// feeds: internalCert returns the configured static leaf, or an error when none
// is configured (so the caller falls through to the public certificate).
func TestInternalCertPrecedence(t *testing.T) {
	want := &tls.Certificate{}
	s := internalServer("https://rel-control.ns.svc:80", "rel-data.ns.svc:443", 0)
	s.cfg.InternalTLSCert = want

	got, err := s.internalCert()
	if err != nil {
		t.Fatalf("internalCert: %v", err)
	}
	if got != want {
		t.Errorf("internalCert = %p, want the configured static leaf %p", got, want)
	}

	// Unconfigured: the caller must be able to detect "no internal cert" and
	// fall back to the public cert (the routing decision is matchesInternalSNI).
	s.cfg.InternalTLSCert = nil
	if _, err := s.internalCert(); err == nil {
		t.Error("internalCert with no provider/cert = nil error, want error")
	}
}
