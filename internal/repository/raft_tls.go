package repository

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/disaster37/goca"
	"github.com/hashicorp/raft"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// nodeCertSafetyMargin is how close a leaf cert may be to expiry before it is
// re-issued at startup (ADR-016: v1 uses a fixed 7-day margin).
const nodeCertSafetyMargin = 7 * 24 * time.Hour

// RaftTLSConfig is the TLS subset of domain.RaftConfig, passed to the builder.
type RaftTLSConfig struct {
	Enabled      bool
	Dir          string        // default <database.dir>/tls
	Validity     time.Duration // default 8760h (1y)
	Organization string        // default "dagger-kubernetes-raft"
	CACertPath   string        // manual mode: CA cert file
	CertPath     string        // manual mode: leaf cert file
	KeyPath      string        // manual mode: leaf key file
	CASecret     string        // auto/K8s mode: K8s Secret name for CA sharing
	CABootstrap  bool          // auto/K8s mode: this node generates+writes the CA
	ClientAuth   bool          // default true (mTLS)
	// SecretPollTimeout bounds the non-bootstrap nodes' poll for the CA Secret
	// (mirrors raft.leader_wait_timeout). 0 = 30s default.
	SecretPollTimeout time.Duration
}

// raftTLSMaterial is the loaded CA pool + leaf certificate used to build the
// transport tls.Config.
type raftTLSMaterial struct {
	caPool *x509.CertPool
	leaf   tls.Certificate
}

// LoadOrBuildRaftTLS prepares the raft TLS material per the selected mode:
//   - manual: load CA + leaf from the configured paths.
//   - auto/K8s: ensure the CA Secret exists (bootstrap node creates it via
//     goca), read it, then issue/reuse this node's leaf at <dir>/tls/node.*.
//   - local-only: generate CA + leaf locally (single-node only).
//
// Returns the material + the tls.Config for the raft StreamLayer.
func LoadOrBuildRaftTLS(
	cfg *RaftTLSConfig,
	isMultiNode bool,
	clientset kubernetes.Interface,
	namespace string,
	dnsNames []string,
	ipAddrs []net.IP,
	commonName string,
	logger *logrus.Logger,
) (*raftTLSMaterial, *tls.Config, error) {
	if !cfg.Enabled {
		return nil, nil, nil
	}
	if cfg.Validity == 0 {
		cfg.Validity = 8760 * time.Hour
	}
	if cfg.Organization == "" {
		cfg.Organization = "dagger-kubernetes-raft"
	}

	// Manual mode: operator pre-provisions CA + leaf PEM files.
	if cfg.CACertPath != "" || cfg.CertPath != "" || cfg.KeyPath != "" {
		return loadManualRaftTLS(cfg)
	}

	caCertPEM, caKeyPEM, err := ensureRaftCA(cfg, clientset, namespace, logger)
	if err != nil {
		return nil, nil, err
	}

	// Local-only mode cannot form a multi-node cluster: there is no way to
	// share the CA across pods.
	if (clientset == nil || cfg.CASecret == "") && isMultiNode {
		return nil, nil, fmt.Errorf("raft TLS auto-mode requires K8s or manual CA files for multi-node")
	}

	ca, err := NewMintingCAFromPEM(caCertPEM, caKeyPEM, cfg.Validity)
	if err != nil {
		return nil, nil, fmt.Errorf("raft TLS: load CA: %w", err)
	}

	leafCertPEM, leafKeyPEM, err := issueOrReuseNodeCert(cfg, ca, commonName, cfg.Organization, dnsNames, ipAddrs)
	if err != nil {
		return nil, nil, err
	}

	m, err := buildRaftTLSMaterial(caCertPEM, leafCertPEM, leafKeyPEM)
	if err != nil {
		return nil, nil, err
	}
	return m, buildRaftTLSConfig(m, cfg.ClientAuth), nil
}

// loadManualRaftTLS loads CA + leaf from the configured manual paths.
func loadManualRaftTLS(cfg *RaftTLSConfig) (*raftTLSMaterial, *tls.Config, error) {
	if cfg.CACertPath == "" || cfg.CertPath == "" || cfg.KeyPath == "" {
		return nil, nil, fmt.Errorf("raft.tls: ca_cert/cert/key must all be set together")
	}
	caCertPEM, err := readPEM(cfg.CACertPath, "CA cert")
	if err != nil {
		return nil, nil, err
	}
	leafCertPEM, err := readPEM(cfg.CertPath, "leaf cert")
	if err != nil {
		return nil, nil, err
	}
	leafKeyPEM, err := readPEM(cfg.KeyPath, "leaf key")
	if err != nil {
		return nil, nil, err
	}
	m, err := buildRaftTLSMaterial(caCertPEM, leafCertPEM, leafKeyPEM)
	if err != nil {
		return nil, nil, err
	}
	return m, buildRaftTLSConfig(m, cfg.ClientAuth), nil
}

// ensureRaftCA returns the CA cert+key PEM, creating it on the bootstrap node
// and sharing via a K8s Secret, or loading from disk in manual/local mode.
func ensureRaftCA(
	cfg *RaftTLSConfig,
	clientset kubernetes.Interface,
	namespace string,
	logger *logrus.Logger,
) (caCertPEM, caKeyPEM []byte, err error) {
	if clientset != nil && cfg.CASecret != "" {
		return ensureRaftCAFromSecret(cfg, clientset, namespace, logger)
	}
	return ensureLocalRaftCA(cfg)
}

// ensureRaftCAFromSecret shares the CA via a K8s Secret: the bootstrap node
// generates + writes it, other nodes poll until it appears.
func ensureRaftCAFromSecret(cfg *RaftTLSConfig, clientset kubernetes.Interface, namespace string, logger *logrus.Logger) (caCertPEM, caKeyPEM []byte, err error) {
	if cfg.CABootstrap {
		certPEM, keyPEM, err := createRaftCAWithGoca("dagger-kubernetes-raft-ca", cfg.Organization)
		if err != nil {
			return nil, nil, err
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: cfg.CASecret, Namespace: namespace},
			Data: map[string][]byte{
				"ca.crt": certPEM,
				"ca.key": keyPEM,
			},
		}
		if _, err := clientset.CoreV1().Secrets(namespace).Create(context.Background(), secret, metav1.CreateOptions{}); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return nil, nil, fmt.Errorf("raft TLS: create CA secret %s: %w", cfg.CASecret, err)
			}
			// The Secret already exists (e.g. the bootstrap pod restarted).
			// Reuse the existing CA rather than overwriting it, so every peer
			// keeps trusting the same internal CA across restarts.
			logger.WithField("secret", cfg.CASecret).Info("raft TLS: CA secret already exists; reusing existing CA")
			return readRaftCASecret(clientset, namespace, cfg.CASecret)
		}
		logger.WithField("secret", cfg.CASecret).Info("raft TLS: generated and shared internal CA via Secret")
		return certPEM, keyPEM, nil
	}

	timeout := cfg.SecretPollTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		certPEM, keyPEM, err := readRaftCASecret(clientset, namespace, cfg.CASecret)
		if err == nil {
			return certPEM, keyPEM, nil
		}
		if !apierrors.IsNotFound(err) {
			// A hard error (RBAC denied, malformed secret) should not be
			// retried as if the secret had not appeared yet.
			return nil, nil, fmt.Errorf("raft TLS: read CA secret %s: %w", cfg.CASecret, err)
		}
		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("raft TLS: CA secret %s did not appear within %s: %w", cfg.CASecret, timeout, err)
		}
		logger.WithField("secret", cfg.CASecret).Debug("raft TLS: waiting for CA secret")
		time.Sleep(500 * time.Millisecond)
	}
}

// readRaftCASecret fetches the CA cert+key from the shared K8s Secret and
// validates that both keys are present.
func readRaftCASecret(clientset kubernetes.Interface, namespace, name string) (caCertPEM, caKeyPEM []byte, err error) {
	secret, err := clientset.CoreV1().Secrets(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	certPEM, okCert := secret.Data["ca.crt"]
	keyPEM, okKey := secret.Data["ca.key"]
	if !okCert || !okKey {
		return nil, nil, fmt.Errorf("raft TLS: CA secret %s is missing ca.crt/ca.key keys", name)
	}
	return certPEM, keyPEM, nil
}

// ensureTLSDir creates the TLS material directory with 0700 permissions,
// tightening an existing directory (e.g. a mounted PVC) if needed.
func ensureTLSDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// #nosec G302 -- dir is a directory; 0700 is more restrictive than the 0750 directory threshold.
	return os.Chmod(dir, 0o700)
}

// ensureLocalRaftCA loads/creates the CA files under <dir>/tls for the
// local-only (single-node) mode.
func ensureLocalRaftCA(cfg *RaftTLSConfig) (caCertPEM, caKeyPEM []byte, err error) {
	if err := ensureTLSDir(cfg.Dir); err != nil {
		return nil, nil, fmt.Errorf("raft TLS: mkdir %s: %w", cfg.Dir, err)
	}
	certPath := filepath.Join(cfg.Dir, "ca.crt")
	keyPath := filepath.Join(cfg.Dir, "ca.key")
	if fileExists(certPath) && fileExists(keyPath) {
		certPEM, err := readPEM(certPath, "CA cert")
		if err != nil {
			return nil, nil, err
		}
		keyPEM, err := readPEM(keyPath, "CA key")
		if err != nil {
			return nil, nil, err
		}
		return certPEM, keyPEM, nil
	}

	certPEM, keyPEM, err := createRaftCAWithGoca("dagger-kubernetes-raft-ca", cfg.Organization)
	if err != nil {
		return nil, nil, err
	}
	if err := writePEM(certPath, certPEM, "CA cert"); err != nil {
		return nil, nil, err
	}
	if err := writePEM(keyPath, keyPEM, "CA key"); err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}

// readPEM reads a PEM file at path, wrapping failures with the labeled context.
func readPEM(path, what string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // paths from config
	if err != nil {
		return nil, fmt.Errorf("raft TLS: read %s: %w", what, err)
	}
	return data, nil
}

// writePEM writes data to path with 0600 permissions, wrapping failures with
// the labeled context.
func writePEM(path string, data []byte, what string) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("raft TLS: write %s: %w", what, err)
	}
	return nil
}

// createRaftCAWithGoca builds a new self-signed CA via goca and returns PEM
// cert+key. Uses the exact API already used in ca_providers.go.
func createRaftCAWithGoca(name, organization string) (certPEM, keyPEM []byte, err error) {
	identity := goca.Identity{
		Organization:       organization,
		OrganizationalUnit: "engineering",
		Country:            "US",
		Locality:           "San Francisco",
		Province:           "California",
	}
	caInstance, err := goca.New(name, identity)
	if err != nil {
		return nil, nil, fmt.Errorf("raft TLS: create goca CA: %w", err)
	}
	return []byte(caInstance.GetCertificate()), []byte(caInstance.GetPrivateKey()), nil
}

// issueOrReuseNodeCert issues a per-node leaf cert signed by the CA, persisted
// at <dir>/tls/node.crt + node.key (0600). Reused if present and not within
// the expiry safety margin.
func issueOrReuseNodeCert(
	cfg *RaftTLSConfig,
	ca *MintingCA,
	commonName, organization string,
	dnsNames []string,
	ipAddrs []net.IP,
) (certPEM, keyPEM []byte, err error) {
	if err := ensureTLSDir(cfg.Dir); err != nil {
		return nil, nil, fmt.Errorf("raft TLS: mkdir %s: %w", cfg.Dir, err)
	}
	certPath := filepath.Join(cfg.Dir, "node.crt")
	keyPath := filepath.Join(cfg.Dir, "node.key")

	if fileExists(certPath) && fileExists(keyPath) {
		existingCert, err := readPEM(certPath, "node cert")
		if err == nil {
			if cert, parseErr := parsePEMCert(existingCert); parseErr == nil && time.Until(cert.NotAfter) > nodeCertSafetyMargin {
				key, keyErr := readPEM(keyPath, "node key")
				if keyErr == nil {
					return existingCert, key, nil
				}
			}
		}
	}

	certPEM, keyPEM, err = ca.IssuePeerCertificate(commonName, organization, dnsNames, ipAddrs, cfg.Validity)
	if err != nil {
		return nil, nil, fmt.Errorf("raft TLS: issue node cert: %w", err)
	}
	if err := writePEM(certPath, certPEM, "node cert"); err != nil {
		return nil, nil, err
	}
	if err := writePEM(keyPath, keyPEM, "node key"); err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}

// buildRaftTLSMaterial assembles the CA pool + leaf tls.Certificate.
func buildRaftTLSMaterial(caCertPEM, leafCertPEM, leafKeyPEM []byte) (*raftTLSMaterial, error) {
	leaf, err := tls.X509KeyPair(leafCertPEM, leafKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("raft TLS: parse leaf key pair: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("raft TLS: no CA cert found in CA PEM")
	}
	return &raftTLSMaterial{caPool: pool, leaf: leaf}, nil
}

// buildRaftTLSConfig assembles the *tls.Config for the raft StreamLayer.
func buildRaftTLSConfig(m *raftTLSMaterial, clientAuth bool) *tls.Config {
	clientAuthType := tls.NoClientCert
	if clientAuth {
		clientAuthType = tls.RequireAndVerifyClientCert
	}
	return &tls.Config{
		Certificates: []tls.Certificate{m.leaf},
		RootCAs:      m.caPool,
		ClientCAs:    m.caPool,
		ClientAuth:   clientAuthType,
		MinVersion:   tls.VersionTLS12,
	}
}

// parsePEMCert parses the first CERTIFICATE PEM block.
func parsePEMCert(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	return x509.ParseCertificate(block.Bytes)
}

// tlsStreamLayer implements raft.StreamLayer over a TLS-wrapped net.Listener.
type tlsStreamLayer struct {
	listener  net.Listener
	config    *tls.Config
	advertise net.Addr
}

var _ raft.StreamLayer = (*tlsStreamLayer)(nil)

// newTLSStreamLayer binds a TLS listener on bindAddr. advertise is the
// externally routable address (nil = use the bound address).
func newTLSStreamLayer(bindAddr string, advertise net.Addr, config *tls.Config) (*tlsStreamLayer, net.Addr, error) {
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("raft TLS: listen %s: %w", bindAddr, err)
	}
	if advertise == nil {
		advertise = ln.Addr()
	}
	return &tlsStreamLayer{listener: ln, config: config, advertise: advertise}, advertise, nil
}

func (l *tlsStreamLayer) Accept() (net.Conn, error) {
	conn, err := l.listener.Accept()
	if err != nil {
		return nil, err
	}
	return tls.Server(conn, l.config), nil
}

func (l *tlsStreamLayer) Dial(addr raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	return tls.DialWithDialer(d, "tcp", string(addr), l.config)
}

func (l *tlsStreamLayer) Close() error {
	return l.listener.Close()
}

func (l *tlsStreamLayer) Addr() net.Addr {
	return l.advertise
}
