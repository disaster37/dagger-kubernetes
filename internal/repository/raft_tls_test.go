package repository

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func testRaftTLSCfg(dir string) *RaftTLSConfig {
	return &RaftTLSConfig{
		Enabled:      true,
		Dir:          dir,
		Validity:     8760 * time.Hour,
		Organization: "dagger-kubernetes-raft",
		ClientAuth:   true,
	}
}

func TestCreateRaftCAWithGoca(t *testing.T) {
	certPEM, keyPEM, err := createRaftCAWithGoca("test-ca", "test-org")
	if err != nil {
		t.Fatalf("createRaftCAWithGoca: %v", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		t.Fatal("empty PEM output")
	}
	cert, err := parsePEMCert(certPEM)
	if err != nil {
		t.Fatalf("parsePEMCert: %v", err)
	}
	if !cert.IsCA {
		t.Fatal("goca CA cert should have IsCA=true")
	}
	if cert.Subject.Organization[0] != "test-org" {
		t.Fatalf("organization = %v, want test-org", cert.Subject.Organization)
	}
}

func TestLoadOrBuildRaftTLSManualMode(t *testing.T) {
	dir := t.TempDir()
	caCertPEM, caKeyPEM, err := createRaftCAWithGoca("ca", "org")
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	ca, err := NewMintingCAFromPEM(caCertPEM, caKeyPEM, time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCAFromPEM: %v", err)
	}
	leafCertPEM, leafKeyPEM, err := ca.IssuePeerCertificate("node-0", "org", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, time.Hour)
	if err != nil {
		t.Fatalf("IssuePeerCertificate: %v", err)
	}

	caPath := filepath.Join(dir, "ca.crt")
	certPath := filepath.Join(dir, "node.crt")
	keyPath := filepath.Join(dir, "node.key")
	if err := os.WriteFile(caPath, caCertPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	if err := os.WriteFile(certPath, leafCertPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, leafKeyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	m, cfg, err := LoadOrBuildRaftTLS(&RaftTLSConfig{
		Enabled:      true,
		Organization: "org",
		CACertPath:   caPath,
		CertPath:     certPath,
		KeyPath:      keyPath,
		ClientAuth:   true,
	}, false, nil, "", nil, nil, "node-0", testLogger())
	if err != nil {
		t.Fatalf("loadOrBuildRaftTLS: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %d, want TLS1.2", cfg.MinVersion)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("Certificates = %d, want 1", len(cfg.Certificates))
	}
	//nolint:staticcheck // caPool is built manually, not from SystemCertPool, so Subjects() is accurate here.
	if m.caPool == nil || len(m.caPool.Subjects()) == 0 {
		t.Fatal("CA pool empty")
	}
}

func TestLoadOrBuildRaftTLSManualModeMissingFiles(t *testing.T) {
	if _, _, err := LoadOrBuildRaftTLS(&RaftTLSConfig{Enabled: true, CACertPath: "/nonexistent/ca.crt"}, false, nil, "", nil, nil, "n", testLogger()); err == nil {
		t.Fatal("expected error for missing manual files")
	}
}

func TestLoadOrBuildRaftTLSLocalOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := testRaftTLSCfg(dir)
	cn := "node-0"
	dns, ips := PodSANs(&RaftDiscoveryConfig{HeadlessService: "headless", Namespace: "ns", ClusterDomain: "cluster.local"}, cn)

	_, tlsCfg, err := LoadOrBuildRaftTLS(cfg, false, nil, "", dns, ips, cn, testLogger())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if tlsCfg == nil {
		t.Fatal("nil tls config")
	}

	// Second call reuses the same leaf (files exist).
	firstCert, _ := os.ReadFile(filepath.Join(dir, "node.crt"))
	_, _, err = LoadOrBuildRaftTLS(cfg, false, nil, "", dns, ips, cn, testLogger())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	secondCert, _ := os.ReadFile(filepath.Join(dir, "node.crt"))
	if !bytes.Equal(firstCert, secondCert) {
		t.Fatal("leaf cert should be reused when present and not expired")
	}

	// Deleting node.crt forces re-issue.
	if err := os.Remove(filepath.Join(dir, "node.crt")); err != nil {
		t.Fatalf("remove node.crt: %v", err)
	}
	_, _, err = LoadOrBuildRaftTLS(cfg, false, nil, "", dns, ips, cn, testLogger())
	if err != nil {
		t.Fatalf("reissue: %v", err)
	}
	thirdCert, _ := os.ReadFile(filepath.Join(dir, "node.crt"))
	if bytes.Equal(firstCert, thirdCert) {
		t.Fatal("leaf cert should be re-issued after deletion")
	}
}

func TestLoadOrBuildRaftTLSLocalOnlyMultiNodeRejected(t *testing.T) {
	cfg := testRaftTLSCfg(t.TempDir())
	_, _, err := LoadOrBuildRaftTLS(cfg, true, nil, "", nil, nil, "node-0", testLogger())
	if err == nil {
		t.Fatal("expected error for multi-node local-only TLS")
	}
}

func TestIssueOrReuseNodeCertExpiryReissue(t *testing.T) {
	dir := t.TempDir()
	caCertPEM, caKeyPEM, err := createRaftCAWithGoca("ca", "org")
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	ca, err := NewMintingCAFromPEM(caCertPEM, caKeyPEM, time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCAFromPEM: %v", err)
	}

	cfg := testRaftTLSCfg(dir)
	dns, ips := PodSANs(&RaftDiscoveryConfig{}, "node-0")

	// Issue a short-TTL cert (expires within the 7-day safety margin).
	cfg.Validity = time.Minute
	_, _, err = issueOrReuseNodeCert(cfg, ca, "node-0", "org", dns, ips)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	firstCert, _ := os.ReadFile(filepath.Join(dir, "node.crt"))

	// Second call: cert is within the safety margin → re-issued.
	_, _, err = issueOrReuseNodeCert(cfg, ca, "node-0", "org", dns, ips)
	if err != nil {
		t.Fatalf("reissue: %v", err)
	}
	secondCert, _ := os.ReadFile(filepath.Join(dir, "node.crt"))
	if bytes.Equal(firstCert, secondCert) {
		t.Fatal("within-margin leaf should be re-issued")
	}

	// Verify SANs are present.
	parsed, err := parsePEMCert(secondCert)
	if err != nil {
		t.Fatalf("parsePEMCert: %v", err)
	}
	if len(parsed.DNSNames) != len(dns) {
		t.Fatalf("DNSNames = %v, want %v", parsed.DNSNames, dns)
	}
	if len(parsed.IPAddresses) != 1 || !parsed.IPAddresses[0].Equal(ips[0]) {
		t.Fatalf("IPAddresses = %v, want %v", parsed.IPAddresses, ips)
	}
}

func TestIssueOrReuseNodeCertReissueOnCAChange(t *testing.T) {
	dir := t.TempDir()
	dns, ips := PodSANs(&RaftDiscoveryConfig{}, "node-0")

	caAPEM, keyAPEM, err := createRaftCAWithGoca("ca", "org")
	if err != nil {
		t.Fatalf("create CA A: %v", err)
	}
	caA, err := NewMintingCAFromPEM(caAPEM, keyAPEM, time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCAFromPEM A: %v", err)
	}
	if _, _, err := issueOrReuseNodeCert(testRaftTLSCfg(dir), caA, "node-0", "org", dns, ips); err != nil {
		t.Fatalf("issue under CA A: %v", err)
	}
	firstCert, _ := os.ReadFile(filepath.Join(dir, "node.crt"))

	// A re-created CA (simulated Secret deletion + rebootstrap) must NOT
	// reuse a leaf signed by the old CA: peers trusting the new CA would
	// reject it (CWE-295 trust split).
	caBPEM, keyBPEM, err := createRaftCAWithGoca("ca", "org")
	if err != nil {
		t.Fatalf("create CA B: %v", err)
	}
	caB, err := NewMintingCAFromPEM(caBPEM, keyBPEM, time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCAFromPEM B: %v", err)
	}
	if _, _, err := issueOrReuseNodeCert(testRaftTLSCfg(dir), caB, "node-0", "org", dns, ips); err != nil {
		t.Fatalf("reissue under CA B: %v", err)
	}
	secondCert, _ := os.ReadFile(filepath.Join(dir, "node.crt"))
	if bytes.Equal(firstCert, secondCert) {
		t.Fatal("leaf signed by the old CA must be re-issued when the CA changed")
	}
	parsed, err := parsePEMCert(secondCert)
	if err != nil {
		t.Fatalf("parsePEMCert: %v", err)
	}
	if err := parsed.CheckSignatureFrom(caB.CACertificate()); err != nil {
		t.Fatalf("re-issued leaf does not chain to the current CA: %v", err)
	}
	if err := parsed.CheckSignatureFrom(caA.CACertificate()); err == nil {
		t.Fatal("re-issued leaf must not chain to the old CA")
	}
}

func TestIssueOrReuseNodeCertReissueOnSANChange(t *testing.T) {
	dir := t.TempDir()
	caCertPEM, caKeyPEM, err := createRaftCAWithGoca("ca", "org")
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	ca, err := NewMintingCAFromPEM(caCertPEM, caKeyPEM, time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCAFromPEM: %v", err)
	}

	// Advertised URI form without the cluster suffix (.svc).
	oldDNS, oldIPs := PodSANs(&RaftDiscoveryConfig{HeadlessService: "headless", Namespace: "ns", ClusterDomain: ""}, "node-0")
	if _, _, err := issueOrReuseNodeCert(testRaftTLSCfg(dir), ca, "node-0", "org", oldDNS, oldIPs); err != nil {
		t.Fatalf("issue (.svc form): %v", err)
	}
	firstCert, _ := os.ReadFile(filepath.Join(dir, "node.crt"))

	// The URI form changed (.svc → .svc.cluster.local): the persisted cert
	// no longer covers the names peers will dial, so it must be re-issued.
	newDNS, newIPs := PodSANs(&RaftDiscoveryConfig{HeadlessService: "headless", Namespace: "ns", ClusterDomain: "cluster.local"}, "node-0")
	if _, _, err := issueOrReuseNodeCert(testRaftTLSCfg(dir), ca, "node-0", "org", newDNS, newIPs); err != nil {
		t.Fatalf("reissue (cluster.local form): %v", err)
	}
	secondCert, _ := os.ReadFile(filepath.Join(dir, "node.crt"))
	if bytes.Equal(firstCert, secondCert) {
		t.Fatal("leaf covering the old URI form must be re-issued when the URI form changed")
	}
	parsed, err := parsePEMCert(secondCert)
	if err != nil {
		t.Fatalf("parsePEMCert: %v", err)
	}
	if !coversSANs(parsed, newDNS, newIPs) {
		t.Fatalf("re-issued SANs = %v / %v, want %v / %v", parsed.DNSNames, parsed.IPAddresses, newDNS, newIPs)
	}
}

func TestIssueOrReuseNodeCertReuseOnExtraSANs(t *testing.T) {
	dir := t.TempDir()
	caCertPEM, caKeyPEM, err := createRaftCAWithGoca("ca", "org")
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	ca, err := NewMintingCAFromPEM(caCertPEM, caKeyPEM, time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCAFromPEM: %v", err)
	}

	// Old code issued the cert with the full FQDN SAN set (.svc.cluster.local).
	oldDNS, oldIPs := PodSANs(&RaftDiscoveryConfig{HeadlessService: "headless", Namespace: "ns", ClusterDomain: "cluster.local"}, "node-0")
	if _, _, err := issueOrReuseNodeCert(testRaftTLSCfg(dir), ca, "node-0", "org", oldDNS, oldIPs); err != nil {
		t.Fatalf("issue (.svc.cluster.local form): %v", err)
	}
	firstCert, _ := os.ReadFile(filepath.Join(dir, "node.crt"))

	// The advertised URI form shrank (.svc.cluster.local → .svc): the
	// persisted cert still covers every required name, so it must be REUSED —
	// re-issuing without the FQDN SAN would break a rolling upgrade where
	// older peers still dial the FQDN and verify the cert against it.
	newDNS, newIPs := PodSANs(&RaftDiscoveryConfig{HeadlessService: "headless", Namespace: "ns", ClusterDomain: ""}, "node-0")
	secondCert, _, err := issueOrReuseNodeCert(testRaftTLSCfg(dir), ca, "node-0", "org", newDNS, newIPs)
	if err != nil {
		t.Fatalf("reuse (.svc form): %v", err)
	}
	if !bytes.Equal(firstCert, secondCert) {
		t.Fatal("cert covering a superset of the required SANs must be reused")
	}
}

func TestIssueOrReuseNodeCertReissueOnMissingSAN(t *testing.T) {
	dir := t.TempDir()
	caCertPEM, caKeyPEM, err := createRaftCAWithGoca("ca", "org")
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	ca, err := NewMintingCAFromPEM(caCertPEM, caKeyPEM, time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCAFromPEM: %v", err)
	}

	// A cert minted for a bare hostname only.
	if _, _, err := issueOrReuseNodeCert(testRaftTLSCfg(dir), ca, "node-0", "org", []string{"node-0", "localhost"}, []net.IP{net.ParseIP("127.0.0.1")}); err != nil {
		t.Fatalf("issue (bare form): %v", err)
	}
	firstCert, _ := os.ReadFile(filepath.Join(dir, "node.crt"))

	// The required set grew (headless/namespace names): the persisted cert no
	// longer covers what peers will dial → must be re-issued.
	reqDNS, reqIPs := PodSANs(&RaftDiscoveryConfig{HeadlessService: "headless", Namespace: "ns"}, "node-0")
	secondCert, _, err := issueOrReuseNodeCert(testRaftTLSCfg(dir), ca, "node-0", "org", reqDNS, reqIPs)
	if err != nil {
		t.Fatalf("reissue (headless form): %v", err)
	}
	if bytes.Equal(firstCert, secondCert) {
		t.Fatal("cert missing required SANs must be re-issued")
	}
}

func TestBuildRaftTLSConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := testRaftTLSCfg(dir)
	_, tlsCfg, err := LoadOrBuildRaftTLS(cfg, false, nil, "", nil, nil, "node-0", testLogger())
	if err != nil {
		t.Fatalf("loadOrBuildRaftTLS: %v", err)
	}
	if tlsCfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %d", tlsCfg.MinVersion)
	}
	if tlsCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v", tlsCfg.ClientAuth)
	}

	// ClientAuth=false disables mTLS.
	cfg.ClientAuth = false
	_, tlsCfg2, err := LoadOrBuildRaftTLS(cfg, false, nil, "", nil, nil, "node-0", testLogger())
	if err != nil {
		t.Fatalf("loadOrBuildRaftTLS (no client auth): %v", err)
	}
	if tlsCfg2.ClientAuth != tls.NoClientCert {
		t.Fatalf("ClientAuth = %v, want NoClientCert", tlsCfg2.ClientAuth)
	}
}

func TestTLSStreamLayerMTLS(t *testing.T) {
	dir := t.TempDir()
	cfg := testRaftTLSCfg(dir)
	dns, ips := PodSANs(&RaftDiscoveryConfig{}, "node-0")
	_, tlsCfg, err := LoadOrBuildRaftTLS(cfg, false, nil, "", dns, ips, "node-0", testLogger())
	if err != nil {
		t.Fatalf("loadOrBuildRaftTLS: %v", err)
	}

	layer, err := newTLSStreamLayer("127.0.0.1:0", nil, tlsCfg)
	if err != nil {
		t.Fatalf("newTLSStreamLayer: %v", err)
	}
	defer func() { _ = layer.Close() }()

	if layer.Addr() == nil {
		t.Fatal("Addr should be non-nil")
	}

	// Concurrent server accept.
	errCh := make(chan error, 1)
	go func() {
		conn, err := layer.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 4)
		if _, err := conn.Read(buf); err != nil {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	// Dial with matching SAN (127.0.0.1 is in the leaf's IP SANs).
	conn, err := layer.Dial(raft.ServerAddress(layer.Addr().String()), time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = conn.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("server accept/read: %v", err)
	}
}

func TestTLSStreamLayerUntrustedPeerFails(t *testing.T) {
	// Server uses one CA; a client dialing with a different CA fails the
	// mTLS handshake.
	serverDir := t.TempDir()
	serverCfg := testRaftTLSCfg(serverDir)
	_, serverTLS, err := LoadOrBuildRaftTLS(serverCfg, false, nil, "", nil, nil, "node-0", testLogger())
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}
	layer, err := newTLSStreamLayer("127.0.0.1:0", nil, serverTLS)
	if err != nil {
		t.Fatalf("newTLSStreamLayer: %v", err)
	}
	defer func() { _ = layer.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := layer.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 1)
		_, _ = conn.Read(buf)
	}()

	clientDir := t.TempDir()
	_, clientTLS, err := LoadOrBuildRaftTLS(testRaftTLSCfg(clientDir), false, nil, "", nil, nil, "other", testLogger())
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	conn, err := tls.Dial("tcp", layer.Addr().String(), clientTLS)
	if err == nil {
		_ = conn.Close()
		t.Fatal("expected handshake failure with untrusted CA")
	}
	<-done
}

func TestTLSFilesPermissions(t *testing.T) {
	dir := t.TempDir()
	cfg := testRaftTLSCfg(dir)
	_, _, err := LoadOrBuildRaftTLS(cfg, false, nil, "", nil, nil, "node-0", testLogger())
	if err != nil {
		t.Fatalf("loadOrBuildRaftTLS: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("tls dir perm = %o, want 700", perm)
	}
	for _, name := range []string{"ca.crt", "ca.key", "node.crt", "node.key"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s perm = %o, want 600", name, perm)
		}
	}
}

func TestEnsureRaftCASecretBootstrapAndPoll(t *testing.T) {
	logger := testLogger()

	// Bootstrap node creates the secret.
	clientset := fake.NewSimpleClientset()
	cfg := RaftTLSConfig{Enabled: true, Dir: t.TempDir(), Organization: "org", CASecret: "raft-ca", CABootstrap: true, SecretPollTimeout: time.Second}
	certPEM, keyPEM, err := ensureRaftCA(&cfg, clientset, "ns", logger)
	if err != nil {
		t.Fatalf("bootstrap ensureRaftCA: %v", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		t.Fatal("empty CA from secret bootstrap")
	}
	secret, err := clientset.CoreV1().Secrets("ns").Get(context.Background(), "raft-ca", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("secret not created: %v", err)
	}
	if len(secret.Data["ca.crt"]) == 0 || len(secret.Data["ca.key"]) == 0 {
		t.Fatal("secret missing ca.crt/ca.key")
	}

	// Non-bootstrap node polls and reads the existing secret.
	cfg2 := RaftTLSConfig{Enabled: true, Dir: t.TempDir(), Organization: "org", CASecret: "raft-ca", SecretPollTimeout: time.Second}
	certPEM2, keyPEM2, err := ensureRaftCA(&cfg2, clientset, "ns", logger)
	if err != nil {
		t.Fatalf("poll ensureRaftCA: %v", err)
	}
	if !bytes.Equal(certPEM2, certPEM) || !bytes.Equal(keyPEM2, keyPEM) {
		t.Fatal("polled CA differs from bootstrap CA")
	}
}

func TestEnsureRaftCASecretBootstrapRestart(t *testing.T) {
	logger := testLogger()
	clientset := fake.NewSimpleClientset()
	cfg := RaftTLSConfig{Enabled: true, Dir: t.TempDir(), Organization: "org", CASecret: "raft-ca", CABootstrap: true, SecretPollTimeout: time.Second}

	// First boot: the bootstrap node generates and shares the CA.
	firstCert, firstKey, err := ensureRaftCA(&cfg, clientset, "ns", logger)
	if err != nil {
		t.Fatalf("first boot ensureRaftCA: %v", err)
	}

	// Restart of the bootstrap node: the Secret already exists, so the node
	// must reuse the existing CA instead of failing on AlreadyExists (which
	// would crash-loop pod-0 and take the cluster down).
	secondCert, secondKey, err := ensureRaftCA(&cfg, clientset, "ns", logger)
	if err != nil {
		t.Fatalf("restart ensureRaftCA: %v", err)
	}
	if !bytes.Equal(secondCert, firstCert) || !bytes.Equal(secondKey, firstKey) {
		t.Fatal("restart must reuse the existing CA, not generate a new one")
	}
}

func TestEnsureRaftCASecretPollTimeout(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	cfg := RaftTLSConfig{Enabled: true, Dir: t.TempDir(), Organization: "org", CASecret: "missing-ca", SecretPollTimeout: 200 * time.Millisecond}
	if _, _, err := ensureRaftCA(&cfg, clientset, "ns", testLogger()); err == nil {
		t.Fatal("expected timeout when secret never appears")
	}
}

func TestEnsureRaftCASecretMissingKeys(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-ca", Namespace: "ns"},
		Data:       map[string][]byte{"wrong": []byte("x")},
	})
	cfg := RaftTLSConfig{Enabled: true, Dir: t.TempDir(), Organization: "org", CASecret: "bad-ca", SecretPollTimeout: time.Second}
	if _, _, err := ensureRaftCA(&cfg, clientset, "ns", testLogger()); err == nil {
		t.Fatal("expected error for secret missing ca.crt/ca.key")
	}
}

func TestEnsureRaftCALocalReuse(t *testing.T) {
	dir := t.TempDir()
	cfg := testRaftTLSCfg(dir)
	cert1, key1, err := ensureRaftCA(cfg, nil, "", testLogger())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	cert2, key2, err := ensureRaftCA(cfg, nil, "", testLogger())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !bytes.Equal(cert1, cert2) || !bytes.Equal(key1, key2) {
		t.Fatal("local CA should be reused across calls")
	}
}
