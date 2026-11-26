package repository

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/disaster37/go-raft/pki"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestEmbeddedProviderCreateCA(t *testing.T) {
	dir, err := os.MkdirTemp("", "ca-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)

	p := NewEmbeddedProvider(dir, 2*time.Hour)

	ca, err := p.MintingCA()
	if err != nil {
		t.Fatalf("MintingCA: %v", err)
	}

	sc, err := ca.MintClientCert("embedded-client")
	if err != nil {
		t.Fatalf("MintClientCert: %v", err)
	}

	clientCert, err := x509.ParseCertificate(sc.CertificateChain[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	if clientCert.Subject.CommonName != "embedded-client" {
		t.Fatalf("expected CN=embedded-client, got %s", clientCert.Subject.CommonName)
	}

	roots := ca.CertPool()
	opts := x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if _, err := clientCert.Verify(opts); err != nil {
		t.Fatalf("client cert not trusted: %v", err)
	}

	certPath := dir + "/ca.crt"
	keyPath := dir + "/ca.key"
	serverCertPath := dir + "/server.crt"
	serverKeyPath := dir + "/server.key"

	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		t.Fatal("ca.crt not created")
	}
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		t.Fatal("ca.key not created")
	}

	tlsCert, err := p.ServerTLSCert()
	if err != nil {
		t.Fatalf("ServerTLSCert: %v", err)
	}
	if len(tlsCert.Certificate) == 0 {
		t.Fatal("server cert chain is empty")
	}
	if tlsCert.PrivateKey == nil {
		t.Fatal("server private key is nil")
	}
	if _, err := os.Stat(serverCertPath); os.IsNotExist(err) {
		t.Fatal("server.crt not created")
	}
	if _, err := os.Stat(serverKeyPath); os.IsNotExist(err) {
		t.Fatal("server.key not created")
	}

	ca2, err := p.MintingCA()
	if err != nil {
		t.Fatalf("MintingCA second call: %v", err)
	}

	sc2, err := ca2.MintClientCert("embedded-client-2")
	if err != nil {
		t.Fatalf("MintClientCert from reloaded CA: %v", err)
	}
	clientCert2, err := x509.ParseCertificate(sc2.CertificateChain[0])
	if err != nil {
		t.Fatalf("ParseCertificate from reloaded CA: %v", err)
	}
	if _, err := clientCert2.Verify(opts); err != nil {
		t.Fatalf("reloaded CA client cert not trusted: %v", err)
	}
}

func TestEmbeddedProviderSecretBootstrap(t *testing.T) {
	dir := t.TempDir()
	clientset := fake.NewSimpleClientset()
	p := NewEmbeddedProviderWithSecret(dir, 2*time.Hour, "minting-ca", "ns", clientset, true, time.Second)

	ca, err := p.loadOrCreateCA()
	if err != nil {
		t.Fatalf("loadOrCreateCA: %v", err)
	}
	if ca == nil {
		t.Fatal("nil CA")
	}

	secret, err := clientset.CoreV1().Secrets("ns").Get(context.Background(), "minting-ca", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("secret not created: %v", err)
	}
	if len(secret.Data["ca.crt"]) == 0 || len(secret.Data["ca.key"]) == 0 {
		t.Fatal("secret missing ca.crt/ca.key keys")
	}

	for _, name := range []string{"ca.crt", "ca.key"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("local %s not cached: %v", name, err)
		}
	}
}

func TestEmbeddedProviderSecretReuseOnRestart(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	p1 := NewEmbeddedProviderWithSecret(t.TempDir(), 2*time.Hour, "minting-ca", "ns", clientset, true, time.Second)
	ca1, err := p1.loadOrCreateCA()
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	certPEM1, _, err := ca1.EncodePEM()
	if err != nil {
		t.Fatalf("EncodePEM: %v", err)
	}

	// Restart of the bootstrap node: the Secret already exists, so the node
	// must reuse it rather than fail on AlreadyExists or overwrite it.
	p2 := NewEmbeddedProviderWithSecret(t.TempDir(), 2*time.Hour, "minting-ca", "ns", clientset, true, time.Second)
	ca2, err := p2.loadOrCreateCA()
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	certPEM2, _, err := ca2.EncodePEM()
	if err != nil {
		t.Fatalf("EncodePEM (restart): %v", err)
	}
	if !bytes.Equal(certPEM1, certPEM2) {
		t.Fatal("restart must reuse the existing Secret CA, not mint a new one")
	}
}

func TestEmbeddedProviderSecretNonBootstrapPoll(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	boot := NewEmbeddedProviderWithSecret(t.TempDir(), 2*time.Hour, "minting-ca", "ns", clientset, true, time.Second)
	bootCA, err := boot.loadOrCreateCA()
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	bootPEM, _, err := bootCA.EncodePEM()
	if err != nil {
		t.Fatalf("EncodePEM: %v", err)
	}

	follower := NewEmbeddedProviderWithSecret(t.TempDir(), 2*time.Hour, "minting-ca", "ns", clientset, false, time.Second)
	followerCA, err := follower.loadOrCreateCA()
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	followerPEM, _, err := followerCA.EncodePEM()
	if err != nil {
		t.Fatalf("EncodePEM (poll): %v", err)
	}
	if !bytes.Equal(followerPEM, bootPEM) {
		t.Fatal("polled CA must match the bootstrap CA")
	}
}

func TestEmbeddedProviderSecretAlreadyExistsFallback(t *testing.T) {
	knownCA, err := NewMintingCA(0)
	if err != nil {
		t.Fatalf("NewMintingCA: %v", err)
	}
	knownCert, knownKey, err := knownCA.EncodePEM()
	if err != nil {
		t.Fatalf("EncodePEM: %v", err)
	}
	clientset := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "minting-ca", Namespace: "ns"},
		Data:       map[string][]byte{"ca.crt": knownCert, "ca.key": knownKey},
	})

	p := NewEmbeddedProviderWithSecret(t.TempDir(), 2*time.Hour, "minting-ca", "ns", clientset, true, time.Second)
	ca, err := p.loadOrCreateCA()
	if err != nil {
		t.Fatalf("loadOrCreateCA: %v", err)
	}
	certPEM, _, err := ca.EncodePEM()
	if err != nil {
		t.Fatalf("EncodePEM: %v", err)
	}
	if !bytes.Equal(certPEM, knownCert) {
		t.Fatal("bootstrap must reuse the pre-existing Secret CA, not overwrite it")
	}
}

func TestEmbeddedProviderSecretPollTimeout(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	p := NewEmbeddedProviderWithSecret(t.TempDir(), 2*time.Hour, "missing-ca", "ns", clientset, false, 200*time.Millisecond)
	if _, err := p.loadOrCreateCA(); err == nil {
		t.Fatal("expected timeout when the minting CA secret never appears")
	}
}

func TestEmbeddedProviderSecretPreCreatedEmpty(t *testing.T) {
	// Simulates an older Helm chart that pre-creates the minting-ca Secret
	// with empty ca.crt/ca.key. The bootstrap node must generate a CA and
	// Update the Secret rather than crashing on a PEM-decode error.
	clientset := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "minting-ca", Namespace: "ns"},
		Data:       map[string][]byte{"ca.crt": {}, "ca.key": {}},
	})

	p := NewEmbeddedProviderWithSecret(t.TempDir(), 2*time.Hour, "minting-ca", "ns", clientset, true, time.Second)
	ca, err := p.loadOrCreateCA()
	if err != nil {
		t.Fatalf("loadOrCreateCA with empty pre-created secret: %v", err)
	}
	if ca == nil {
		t.Fatal("nil CA")
	}

	// The Secret must now hold a real CA.
	secret, err := clientset.CoreV1().Secrets("ns").Get(context.Background(), "minting-ca", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get secret: %v", err)
	}
	if len(secret.Data["ca.crt"]) == 0 || len(secret.Data["ca.key"]) == 0 {
		t.Fatal("bootstrap node did not populate the empty minting-ca Secret")
	}

	// A second bootstrap node restart must reuse the now-populated Secret.
	p2 := NewEmbeddedProviderWithSecret(t.TempDir(), 2*time.Hour, "minting-ca", "ns", clientset, true, time.Second)
	ca2, err := p2.loadOrCreateCA()
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	cert1, _, _ := ca.EncodePEM()
	cert2, _, _ := ca2.EncodePEM()
	if !bytes.Equal(cert1, cert2) {
		t.Fatal("restart must reuse the populated Secret CA, not mint a new one")
	}
}

func TestEmbeddedProviderLocalFileFallback(t *testing.T) {
	// A clientset with an empty secret name must fall back to local files.
	dir := t.TempDir()
	p := NewEmbeddedProviderWithSecret(dir, 2*time.Hour, "", "ns", fake.NewSimpleClientset(), true, time.Second)
	ca, err := p.loadOrCreateCA()
	if err != nil {
		t.Fatalf("loadOrCreateCA: %v", err)
	}
	if ca == nil {
		t.Fatal("nil CA")
	}
	for _, name := range []string{"ca.crt", "ca.key"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("local %s not created: %v", name, err)
		}
	}

	// A nil clientset with a secret name also falls back to local files.
	dir2 := t.TempDir()
	p2 := NewEmbeddedProviderWithSecret(dir2, 2*time.Hour, "minting-ca", "ns", nil, true, time.Second)
	if _, err := p2.loadOrCreateCA(); err != nil {
		t.Fatalf("nil clientset loadOrCreateCA: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir2, "ca.crt")); err != nil {
		t.Fatalf("local ca.crt not created (nil clientset): %v", err)
	}
}

func TestFileCAProviderDelegatesMinting(t *testing.T) {
	// The cert-manager/external providers must still auto-bootstrap the
	// minting CA (shared across pods), only delegating the server cert to
	// externally managed PEM files.
	dir := t.TempDir()
	clientset := fake.NewSimpleClientset()
	minting := NewEmbeddedProviderWithSecret(dir, 2*time.Hour, "minting-ca", "ns", clientset, true, time.Second)

	serverCert := filepath.Join(dir, "server.crt")
	serverKey := filepath.Join(dir, "server.key")
	if err := os.WriteFile(serverCert, []byte("cert"), 0o600); err != nil {
		t.Fatalf("write server.crt: %v", err)
	}
	if err := os.WriteFile(serverKey, []byte("key"), 0o600); err != nil {
		t.Fatalf("write server.key: %v", err)
	}

	for _, tc := range []struct {
		name     string
		provider domain.CAProvider
	}{
		{"cert-manager", NewCertManagerProvider(serverCert, serverKey, minting)},
		{"external", NewExternalProvider(serverCert, serverKey, minting)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ca, err := tc.provider.MintingCA()
			if err != nil {
				t.Fatalf("MintingCA: %v", err)
			}
			sc, err := ca.MintClientCert("file-ca-client")
			if err != nil {
				t.Fatalf("MintClientCert: %v", err)
			}
			clientCert, err := x509.ParseCertificate(sc.CertificateChain[0])
			if err != nil {
				t.Fatalf("ParseCertificate: %v", err)
			}
			if _, err := clientCert.Verify(x509.VerifyOptions{
				Roots:     ca.CertPool(),
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			}); err != nil {
				t.Fatalf("client cert not trusted: %v", err)
			}

			// The server cert must still come from the mounted PEM files.
			if _, err := tc.provider.ServerTLSCert(); err == nil {
				t.Fatal("expected ServerTLSCert to fail on invalid PEM, got nil error")
			}
		})
	}

	// The bootstrap node must have written the minting CA to the Secret.
	secret, err := clientset.CoreV1().Secrets("ns").Get(context.Background(), "minting-ca", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("secret not created: %v", err)
	}
	if len(secret.Data["ca.crt"]) == 0 || len(secret.Data["ca.key"]) == 0 {
		t.Fatal("secret missing ca.crt/ca.key keys")
	}
}

func TestEmbeddedProviderServerCertSANs(t *testing.T) {
	const (
		ownFQDN     = "sts-0.headless.ns.svc.cluster.local"
		ownSvcShort = "sts-0.headless.ns.svc"
	)
	dir := t.TempDir()
	p := NewEmbeddedProvider(dir, 2*time.Hour, "data.example.com", ownFQDN, ownSvcShort)
	cert, err := p.ServerTLSCert()
	if err != nil {
		t.Fatalf("ServerTLSCert: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	hostname, _ := os.Hostname()
	requiredDNS := []string{"localhost", hostname, "data.example.com", ownFQDN, ownSvcShort, "supervisor", "supervisor-control", "supervisor-control.dagger-kubernetes.svc"}
	for _, want := range requiredDNS {
		found := false
		for _, dns := range leaf.DNSNames {
			if dns == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing DNS SAN %q in %v", want, leaf.DNSNames)
		}
	}
	for _, dns := range leaf.DNSNames {
		if strings.HasPrefix(dns, "*.") {
			t.Fatalf("embedded server cert must not carry a wildcard SAN, got %q", dns)
		}
	}

	foundIP := false
	for _, ip := range leaf.IPAddresses {
		if ip.Equal(net.ParseIP("127.0.0.1")) {
			foundIP = true
			break
		}
	}
	if !foundIP {
		t.Fatalf("missing 127.0.0.1 IP SAN in %v", leaf.IPAddresses)
	}
}

// TestServerTLSCertReissuesOnSANGrowth proves the cached server certificate is
// transparently re-issued (under the same minting CA) when the required SAN
// set grows — the rollout path that picks up the own-FQDN SANs needed for the
// leader-forward hop.
func TestServerTLSCertReissuesOnSANGrowth(t *testing.T) {
	const (
		ownFQDN     = "sts-0.headless.ns.svc.cluster.local"
		ownSvcShort = "sts-0.headless.ns.svc"
	)
	dir := t.TempDir()

	// First boot: a cert without the own-FQDN SANs is cached.
	p1 := NewEmbeddedProvider(dir, 2*time.Hour, "data.example.com")
	first, err := p1.ServerTLSCert()
	if err != nil {
		t.Fatalf("first ServerTLSCert: %v", err)
	}
	firstLeaf, err := x509.ParseCertificate(first.Certificate[0])
	if err != nil {
		t.Fatalf("parse first cert: %v", err)
	}
	for _, dns := range firstLeaf.DNSNames {
		if dns == ownFQDN || dns == ownSvcShort {
			t.Fatalf("precondition failed: first cert already has %q", dns)
		}
	}

	// Second boot with the own-FQDN SANs added: the stale cache is re-issued.
	p2 := NewEmbeddedProvider(dir, 2*time.Hour, "data.example.com", ownFQDN, ownSvcShort)
	second, err := p2.ServerTLSCert()
	if err != nil {
		t.Fatalf("second ServerTLSCert: %v", err)
	}
	secondLeaf, err := x509.ParseCertificate(second.Certificate[0])
	if err != nil {
		t.Fatalf("parse second cert: %v", err)
	}
	if bytes.Equal(firstLeaf.Raw, secondLeaf.Raw) {
		t.Fatal("expected a fresh certificate after the SAN set grew, got the cached one")
	}
	for _, want := range []string{ownFQDN, ownSvcShort} {
		found := false
		for _, dns := range secondLeaf.DNSNames {
			if dns == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("re-issued cert missing own-FQDN SAN %q in %v", want, secondLeaf.DNSNames)
		}
	}

	// The re-issued cert must be signed by the same shared minting CA.
	ca, err := p2.MintingCA()
	if err != nil {
		t.Fatalf("MintingCA: %v", err)
	}
	if _, err := secondLeaf.Verify(x509.VerifyOptions{
		Roots:     ca.CertPool(),
		DNSName:   ownFQDN,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("re-issued cert not trusted for %s: %v", ownFQDN, err)
	}

	// A third call with the same SAN set must reuse the fresh cert (no churn).
	third, err := p2.ServerTLSCert()
	if err != nil {
		t.Fatalf("third ServerTLSCert: %v", err)
	}
	thirdLeaf, err := x509.ParseCertificate(third.Certificate[0])
	if err != nil {
		t.Fatalf("parse third cert: %v", err)
	}
	if !bytes.Equal(secondLeaf.Raw, thirdLeaf.Raw) {
		t.Fatal("expected the re-issued cert to be reused once the SAN set is covered")
	}
}

// TestEmbeddedProviderInternalServerCert proves the internal-listener leaf is
// minted by the shared minting CA with the injected own-FQDN SANs (no
// wildcard), verifies in a real TLS handshake against the CA pool, and is
// reused from the server-internal.crt/.key cache on the second call.
func TestEmbeddedProviderInternalServerCert(t *testing.T) {
	const ownFQDN = "sts-0.headless.ns.svc.cluster.local"
	dir := t.TempDir()
	p := NewEmbeddedProvider(dir, 2*time.Hour, "data.example.com", ownFQDN)

	cert, err := p.InternalServerTLSCert()
	if err != nil {
		t.Fatalf("InternalServerTLSCert: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	foundFQDN := false
	for _, dns := range leaf.DNSNames {
		if dns == ownFQDN {
			foundFQDN = true
		}
		if strings.HasPrefix(dns, "*.") {
			t.Fatalf("internal leaf must not carry a wildcard SAN, got %q", dns)
		}
	}
	if !foundFQDN {
		t.Fatalf("internal leaf missing own-FQDN SAN %q in %v", ownFQDN, leaf.DNSNames)
	}

	for _, name := range []string{"server-internal.crt", "server-internal.key"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("internal leaf %s not persisted: %v", name, err)
		}
	}

	ca, err := p.MintingCA()
	if err != nil {
		t.Fatalf("MintingCA: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     ca.CertPool(),
		DNSName:   ownFQDN,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("internal leaf not trusted for %s: %v", ownFQDN, err)
	}

	// Real TLS handshake: a client trusting only the minting-CA pool and
	// dialing the leaf's FQDN SAN must complete the handshake.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				if tc, ok := conn.(*tls.Conn); ok {
					_ = tc.Handshake()
				}
			}(conn)
		}
	}()

	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", ln.Addr().String(), &tls.Config{
		RootCAs:    ca.CertPool(),
		ServerName: ownFQDN,
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("TLS handshake with minting-CA pool: %v", err)
	}
	_ = conn.Close()

	// Second call must reuse the cached server-internal pair (no churn).
	again, err := p.InternalServerTLSCert()
	if err != nil {
		t.Fatalf("second InternalServerTLSCert: %v", err)
	}
	againLeaf, err := x509.ParseCertificate(again.Certificate[0])
	if err != nil {
		t.Fatalf("parse second cert: %v", err)
	}
	if !bytes.Equal(leaf.Raw, againLeaf.Raw) {
		t.Fatal("expected the cached internal leaf to be reused")
	}
}

// TestInternalServerCertReissuesOnSANGrowth proves the cached internal leaf is
// transparently re-issued under the same minting CA when the FQDN SAN set
// grows (e.g. a cluster_domain/headless_service change).
func TestInternalServerCertReissuesOnSANGrowth(t *testing.T) {
	const ownFQDN = "sts-0.headless.ns.svc.cluster.local"
	dir := t.TempDir()

	// First boot: a leaf without the own-FQDN SAN is cached.
	p1 := NewEmbeddedProvider(dir, 2*time.Hour, "data.example.com")
	first, err := p1.InternalServerTLSCert()
	if err != nil {
		t.Fatalf("first InternalServerTLSCert: %v", err)
	}
	firstLeaf, err := x509.ParseCertificate(first.Certificate[0])
	if err != nil {
		t.Fatalf("parse first cert: %v", err)
	}
	for _, dns := range firstLeaf.DNSNames {
		if dns == ownFQDN {
			t.Fatalf("precondition failed: first leaf already has %q", dns)
		}
	}

	// Second boot with the own-FQDN SAN added: the stale cache is re-issued.
	p2 := NewEmbeddedProvider(dir, 2*time.Hour, "data.example.com", ownFQDN)
	second, err := p2.InternalServerTLSCert()
	if err != nil {
		t.Fatalf("second InternalServerTLSCert: %v", err)
	}
	secondLeaf, err := x509.ParseCertificate(second.Certificate[0])
	if err != nil {
		t.Fatalf("parse second cert: %v", err)
	}
	if bytes.Equal(firstLeaf.Raw, secondLeaf.Raw) {
		t.Fatal("expected a fresh leaf after the SAN set grew, got the cached one")
	}
	found := false
	for _, dns := range secondLeaf.DNSNames {
		if dns == ownFQDN {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("re-issued leaf missing own-FQDN SAN %q in %v", ownFQDN, secondLeaf.DNSNames)
	}

	// The re-issued leaf must be signed by the same shared minting CA.
	ca, err := p2.MintingCA()
	if err != nil {
		t.Fatalf("MintingCA: %v", err)
	}
	if _, err := secondLeaf.Verify(x509.VerifyOptions{
		Roots:     ca.CertPool(),
		DNSName:   ownFQDN,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("re-issued leaf not trusted for %s: %v", ownFQDN, err)
	}
}

// verifyClientCert reports whether sc verifies against pool at time now.
func verifyClientCert(t *testing.T, sc *domain.SerializableCertificate, pool *x509.CertPool, now time.Time) bool {
	t.Helper()
	cert, err := x509.ParseCertificate(sc.CertificateChain[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	_, err = cert.Verify(x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	return err == nil
}

// TestDynamicMintingCAPhasedRotationPreservesInFlightClientCert proves the
// phased dual-trust rotation keeps an in-flight engine client cert trusted
// during the overlap window and retires it only after the old CA is pruned.
func TestDynamicMintingCAPhasedRotationPreservesInFlightClientCert(t *testing.T) {
	dir := t.TempDir()
	provider := NewEmbeddedProvider(dir, 24*time.Hour)
	sealer, err := DeriveSealer("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("DeriveSealer: %v", err)
	}
	dynamicCA, err := NewDynamicMintingCA(DynamicMintingCAConfig{Provider: provider, Sealer: sealer, Logger: testLogger()})
	if err != nil {
		t.Fatalf("NewDynamicMintingCA: %v", err)
	}

	// C0 minted under M0.
	c0, err := dynamicCA.MintClientCert("engine-0")
	if err != nil {
		t.Fatalf("MintClientCert C0: %v", err)
	}

	store := newTestRaftStore(t)
	store.SetMetaHook(func(key, value string) {
		if key == MintingKeyringMetaKey {
			if err := dynamicCA.SetKeyringJSON(value); err != nil {
				t.Errorf("SetKeyringJSON: %v", err)
			}
		}
	})

	now := time.Now()
	rotator := NewMintingRotator(&MintingRotatorConfig{
		Store:          store,
		CA:             dynamicCA,
		Bootstrap:      provider.BootstrapProvider(),
		Sealer:         sealer,
		ClientCertTTL:  24 * time.Hour,
		RotationPeriod: time.Hour,
		CheckInterval:  time.Second,
		CommonName:     "dagger-kubernetes-minting-ca",
		Organization:   "dagger-kubernetes",
		CAValidity:     30 * 365 * 24 * time.Hour,
		Logger:         testLogger(),
		Now:            func() time.Time { return now },
	})
	ctx := context.Background()

	// Seed (Term 1).
	if err := rotator.RotateOnce(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Stage (Term 2).
	now = now.Add(2 * time.Hour)
	if err := rotator.RotateOnce(ctx); err != nil {
		t.Fatalf("stage: %v", err)
	}
	// Activate (Term 3, active M1, overlap [M0, M1]).
	if err := rotator.RotateOnce(ctx); err != nil {
		t.Fatalf("activate: %v", err)
	}

	// C1 minted under M1 during the overlap.
	c1, err := dynamicCA.MintClientCert("engine-1")
	if err != nil {
		t.Fatalf("MintClientCert C1: %v", err)
	}

	// Client certs are minted with the real clock (goca), so verify against
	// the real clock; the fake clock only drives the rotation decisions.
	realNow := time.Now()
	pool := dynamicCA.CertPool()
	if !verifyClientCert(t, c0, pool, realNow) {
		t.Fatal("C0 (M0) must verify during the dual-trust overlap")
	}
	if !verifyClientCert(t, c1, pool, realNow) {
		t.Fatal("C1 (M1) must verify during the dual-trust overlap")
	}

	// Prune (Term 4) after max(2*clientCertTTL, 1h).
	now = now.Add(2*24*time.Hour + time.Hour)
	if err := rotator.RotateOnce(ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}
	pool = dynamicCA.CertPool()
	if verifyClientCert(t, c0, pool, realNow) {
		t.Fatal("C0 (M0) must not verify after pruning M0")
	}
	c2, err := dynamicCA.MintClientCert("engine-2")
	if err != nil {
		t.Fatalf("MintClientCert C2: %v", err)
	}
	if !verifyClientCert(t, c2, pool, realNow) {
		t.Fatal("a fresh cert under M1 must verify after pruning M0")
	}
}

// TestDynamicMintingCATrustBundlePEMPhasedRotation proves TrustBundlePEM
// publishes the public CA certificates only (never the private keys) and
// tracks the phased-rotation dual-trust window: [M0] before staging, [M0, M1]
// through stage/activate, and [M1] after pruning.
func TestDynamicMintingCATrustBundlePEMPhasedRotation(t *testing.T) {
	dir := t.TempDir()
	provider := NewEmbeddedProvider(dir, 24*time.Hour)
	sealer, err := DeriveSealer("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("DeriveSealer: %v", err)
	}
	dynamicCA, err := NewDynamicMintingCA(DynamicMintingCAConfig{Provider: provider, Sealer: sealer, Logger: testLogger()})
	if err != nil {
		t.Fatalf("NewDynamicMintingCA: %v", err)
	}

	countCerts := func(t *testing.T, pemBytes []byte) int {
		t.Helper()
		block, rest := pem.Decode(pemBytes)
		n := 0
		for block != nil {
			if block.Type == "CERTIFICATE" {
				n++
			}
			block, rest = pem.Decode(rest)
		}
		return n
	}
	assertNoPrivateKey := func(t *testing.T, pemBytes []byte) {
		t.Helper()
		if bytes.Contains(pemBytes, []byte("PRIVATE KEY")) {
			t.Fatal("trust bundle must never contain a PRIVATE KEY block")
		}
	}

	// Pre-keyring bootstrap (keyring == nil, active CA only).
	bundle, ok := dynamicCA.TrustBundlePEM()
	if !ok {
		t.Fatal("pre-keyring TrustBundlePEM must be ok")
	}
	if got := countCerts(t, bundle); got != 1 {
		t.Fatalf("pre-keyring bundle certs = %d, want 1", got)
	}
	assertNoPrivateKey(t, bundle)

	store := newTestRaftStore(t)
	store.SetMetaHook(func(key, value string) {
		if key == MintingKeyringMetaKey {
			if err := dynamicCA.SetKeyringJSON(value); err != nil {
				t.Errorf("SetKeyringJSON: %v", err)
			}
		}
	})

	now := time.Now()
	rotator := NewMintingRotator(&MintingRotatorConfig{
		Store:          store,
		CA:             dynamicCA,
		Bootstrap:      provider.BootstrapProvider(),
		Sealer:         sealer,
		ClientCertTTL:  24 * time.Hour,
		RotationPeriod: time.Hour,
		CheckInterval:  time.Second,
		CommonName:     "dagger-kubernetes-minting-ca",
		Organization:   "dagger-kubernetes",
		CAValidity:     30 * 365 * 24 * time.Hour,
		Logger:         testLogger(),
		Now:            func() time.Time { return now },
	})
	ctx := context.Background()

	// Seed (Term 1): single active CA M0.
	if err := rotator.RotateOnce(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	bundle, ok = dynamicCA.TrustBundlePEM()
	if !ok || countCerts(t, bundle) != 1 {
		t.Fatalf("post-seed bundle = %v certs (ok=%v), want 1", countCerts(t, bundle), ok)
	}
	assertNoPrivateKey(t, bundle)

	// Stage (Term 2): dual-trust [M0, M1].
	now = now.Add(2 * time.Hour)
	if err := rotator.RotateOnce(ctx); err != nil {
		t.Fatalf("stage: %v", err)
	}
	bundle, ok = dynamicCA.TrustBundlePEM()
	if !ok || countCerts(t, bundle) != 2 {
		t.Fatalf("stage bundle = %v certs (ok=%v), want 2", countCerts(t, bundle), ok)
	}
	assertNoPrivateKey(t, bundle)

	// Activate (Term 3): still dual-trust [M0, M1].
	if err := rotator.RotateOnce(ctx); err != nil {
		t.Fatalf("activate: %v", err)
	}
	bundle, ok = dynamicCA.TrustBundlePEM()
	if !ok || countCerts(t, bundle) != 2 {
		t.Fatalf("activate bundle = %v certs (ok=%v), want 2", countCerts(t, bundle), ok)
	}
	assertNoPrivateKey(t, bundle)

	// Prune (Term 4): single active CA M1.
	now = now.Add(2*24*time.Hour + time.Hour)
	if err := rotator.RotateOnce(ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}
	bundle, ok = dynamicCA.TrustBundlePEM()
	if !ok || countCerts(t, bundle) != 1 {
		t.Fatalf("prune bundle = %v certs (ok=%v), want 1", countCerts(t, bundle), ok)
	}
	assertNoPrivateKey(t, bundle)
}

// TestMintingRotatorStageIdempotent proves a crash between the two stage
// writes (Term=2 with AppliedIndex=0) does not strand the rotation: the next
// tick records the applied index and keeps exactly two keys.
func TestMintingRotatorStageIdempotent(t *testing.T) {
	dir := t.TempDir()
	provider := NewEmbeddedProvider(dir, 24*time.Hour)
	sealer, err := DeriveSealer("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("DeriveSealer: %v", err)
	}
	dynamicCA, err := NewDynamicMintingCA(DynamicMintingCAConfig{Provider: provider, Sealer: sealer, Logger: testLogger()})
	if err != nil {
		t.Fatalf("NewDynamicMintingCA: %v", err)
	}
	store := newTestRaftStore(t)
	store.SetMetaHook(func(key, value string) {
		if key == MintingKeyringMetaKey {
			if err := dynamicCA.SetKeyringJSON(value); err != nil {
				t.Errorf("SetKeyringJSON: %v", err)
			}
		}
	})

	now := time.Now()
	rotator := NewMintingRotator(&MintingRotatorConfig{
		Store:          store,
		CA:             dynamicCA,
		Bootstrap:      provider.BootstrapProvider(),
		Sealer:         sealer,
		ClientCertTTL:  24 * time.Hour,
		RotationPeriod: time.Hour,
		CheckInterval:  time.Second,
		CommonName:     "dagger-kubernetes-minting-ca",
		Organization:   "dagger-kubernetes",
		CAValidity:     30 * 365 * 24 * time.Hour,
		Logger:         testLogger(),
		Now:            func() time.Time { return now },
	})
	ctx := context.Background()

	// Seed (Term 1).
	if err := rotator.RotateOnce(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Stage (Term 2) with the clock advanced past the rotation period.
	now = now.Add(2 * time.Hour)
	if err := rotator.RotateOnce(ctx); err != nil {
		t.Fatalf("stage: %v", err)
	}

	// Simulate a crash between the two stage writes: reset the keyring's
	// AppliedIndex to 0 (as if the second write never landed).
	kr, err := rotator.readKeyring()
	if err != nil {
		t.Fatalf("readKeyring: %v", err)
	}
	if kr == nil || len(kr.Keys) != 2 {
		t.Fatalf("expected a staged 2-key keyring, got %+v", kr)
	}
	kr.AppliedIndex = 0
	if err := rotator.writeKeyring(ctx, kr); err != nil {
		t.Fatalf("writeKeyring (simulate crash): %v", err)
	}

	// Re-run the rotation: activate must record the applied index (recovering
	// the crashed stage write) without appending a third key.
	if err := rotator.RotateOnce(ctx); err != nil {
		t.Fatalf("re-run rotation: %v", err)
	}
	kr, err = rotator.readKeyring()
	if err != nil {
		t.Fatalf("readKeyring after re-run: %v", err)
	}
	if len(kr.Keys) != 2 {
		t.Fatalf("re-run appended a duplicate key: got %d keys, want 2", len(kr.Keys))
	}
	if kr.AppliedIndex == 0 {
		t.Fatal("re-run must record the applied index")
	}
}

// TestDataplaneFileWatcherHotReload proves the cert-manager/external data-plane
// server cert hot-reloads renewed files and rejects corrupt PEMs while keeping
// the previous certificate active.
func TestDataplaneFileWatcherHotReload(t *testing.T) {
	dir := t.TempDir()
	ca, err := pki.NewMintingCA("test-ca", "org", 10*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCA: %v", err)
	}
	caCertPEM, _, err := ca.EncodePEM()
	if err != nil {
		t.Fatalf("EncodePEM: %v", err)
	}
	caPath := filepath.Join(dir, "ca.crt")
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	if err := os.WriteFile(caPath, caCertPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	writeLeaf := func() *x509.Certificate {
		certPEM, keyPEM, err := ca.IssuePeerCertificate("server", "org", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, time.Hour)
		if err != nil {
			t.Fatalf("IssuePeerCertificate: %v", err)
		}
		if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
			t.Fatalf("write cert: %v", err)
		}
		if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
			t.Fatalf("write key: %v", err)
		}
		parsed, err := pki.ParsePEMCert(certPEM)
		if err != nil {
			t.Fatalf("ParsePEMCert: %v", err)
		}
		return parsed
	}
	writeLeaf()

	w, err := NewDataplaneFileWatcher(certPath, keyPath, caPath, 50*time.Millisecond, testLogger())
	if err != nil {
		t.Fatalf("NewDataplaneFileWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Start(ctx)
	provider := w.Provider()

	renewed := writeLeaf()
	deadline := time.Now().Add(10 * time.Second)
	reloaded := false
	for time.Now().Before(deadline) {
		cur, err := provider.GetCertificate(nil)
		if err == nil && len(cur.Certificate) > 0 {
			parsed, perr := x509.ParseCertificate(cur.Certificate[0])
			if perr == nil && parsed.SerialNumber.Cmp(renewed.SerialNumber) == 0 {
				reloaded = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !reloaded {
		t.Fatal("file watcher did not hot-reload the renewed certificate")
	}

	// A corrupt PEM must be rejected, keeping the previous certificate active.
	if err := os.WriteFile(certPath, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("write corrupt cert: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	cur, err := provider.GetCertificate(nil)
	if err != nil || len(cur.Certificate) == 0 {
		t.Fatalf("provider lost its certificate after a corrupt reload: %v", err)
	}
	parsed, err := x509.ParseCertificate(cur.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	if parsed.SerialNumber.Cmp(renewed.SerialNumber) != 0 {
		t.Fatal("corrupt PEM replaced the active certificate")
	}
}

// TestFileCAProviderInternalCertDelegatesMinting proves cert-manager/external
// modes mint the internal-listener leaf from the shared minting CA (never
// from the public PEM files): the returned leaf carries the FQDN SAN and
// verifies against the minting-CA pool.
func TestFileCAProviderInternalCertDelegatesMinting(t *testing.T) {
	const ownFQDN = "sts-0.headless.ns.svc.cluster.local"
	dir := t.TempDir()
	minting := NewEmbeddedProvider(dir, 2*time.Hour, ownFQDN)

	serverCert := filepath.Join(dir, "server.crt")
	serverKey := filepath.Join(dir, "server.key")
	if err := os.WriteFile(serverCert, []byte("cert"), 0o600); err != nil {
		t.Fatalf("write server.crt: %v", err)
	}
	if err := os.WriteFile(serverKey, []byte("key"), 0o600); err != nil {
		t.Fatalf("write server.key: %v", err)
	}

	for _, tc := range []struct {
		name     string
		provider domain.CAProvider
	}{
		{"cert-manager", NewCertManagerProvider(serverCert, serverKey, minting)},
		{"external", NewExternalProvider(serverCert, serverKey, minting)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert, err := tc.provider.InternalServerTLSCert()
			if err != nil {
				t.Fatalf("InternalServerTLSCert: %v", err)
			}
			leaf, err := x509.ParseCertificate(cert.Certificate[0])
			if err != nil {
				t.Fatalf("ParseCertificate: %v", err)
			}
			found := false
			for _, dns := range leaf.DNSNames {
				if dns == ownFQDN {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("%s mode internal leaf missing FQDN SAN %q in %v", tc.name, ownFQDN, leaf.DNSNames)
			}
			ca, err := tc.provider.MintingCA()
			if err != nil {
				t.Fatalf("MintingCA: %v", err)
			}
			if _, err := leaf.Verify(x509.VerifyOptions{
				Roots:     ca.CertPool(),
				DNSName:   ownFQDN,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			}); err != nil {
				t.Fatalf("%s mode internal leaf not trusted for %s: %v", tc.name, ownFQDN, err)
			}
		})
	}
}
