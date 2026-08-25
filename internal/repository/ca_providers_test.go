package repository

import (
	"bytes"
	"context"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	knownCert, knownKey, err := newMintingCAWithGoca()
	if err != nil {
		t.Fatalf("newMintingCAWithGoca: %v", err)
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
	dir := t.TempDir()
	p := NewEmbeddedProvider(dir, 2*time.Hour, "data.example.com")
	cert, err := p.ServerTLSCert()
	if err != nil {
		t.Fatalf("ServerTLSCert: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	hostname, _ := os.Hostname()
	requiredDNS := []string{"localhost", hostname, "data.example.com", "supervisor", "supervisor-control", "supervisor-control.dagger-kubernetes.svc"}
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
