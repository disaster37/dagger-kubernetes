package repository

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/disaster37/goca"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type EmbeddedProvider struct {
	caPath        string
	clientCertTTL time.Duration
	extraSANs     []string

	// K8s Secret-backed minting CA sharing (multi-node). A nil clientset or
	// empty secretName falls back to the local-file behavior.
	secretName        string
	namespace         string
	clientset         kubernetes.Interface
	caBootstrap       bool
	secretPollTimeout time.Duration
}

var _ domain.CAProvider = (*EmbeddedProvider)(nil)

func NewEmbeddedProvider(caPath string, clientCertTTL time.Duration, extraSANs ...string) *EmbeddedProvider {
	return &EmbeddedProvider{
		caPath:        caPath,
		clientCertTTL: clientCertTTL,
		extraSANs:     extraSANs,
	}
}

// NewEmbeddedProviderWithSecret returns an EmbeddedProvider that shares the
// minting CA across pods via a Kubernetes Secret (ca.minting_ca_secret),
// mirroring the raft CA Secret pattern in raft_tls.go. The CA private key is
// stored in the Secret because any pod may mint engine client certs — the
// Secret must be RBAC-restricted to the supervisor ServiceAccount. A local
// copy of ca.crt/ca.key is still cached under caPath (0600, dir 0700).
func NewEmbeddedProviderWithSecret(caPath string, clientCertTTL time.Duration, secretName, namespace string, clientset kubernetes.Interface, caBootstrap bool, secretPollTimeout time.Duration, extraSANs ...string) *EmbeddedProvider {
	return &EmbeddedProvider{
		caPath:            caPath,
		clientCertTTL:     clientCertTTL,
		extraSANs:         extraSANs,
		secretName:        secretName,
		namespace:         namespace,
		clientset:         clientset,
		caBootstrap:       caBootstrap,
		secretPollTimeout: secretPollTimeout,
	}
}

func (p *EmbeddedProvider) MintingCA() (domain.MintingCA, error) {
	return p.loadOrCreateCA()
}

func (p *EmbeddedProvider) ServerTLSCert() (tls.Certificate, error) {
	ca, err := p.loadOrCreateCA()
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("minting CA: %w", err)
	}

	serverCertPath := filepath.Join(p.caPath, "server.crt")
	serverKeyPath := filepath.Join(p.caPath, "server.key")

	if fileExists(serverCertPath) && fileExists(serverKeyPath) {
		return loadTLSKeyPair(serverCertPath, serverKeyPath)
	}

	return p.issueServerCert(ca, serverCertPath, serverKeyPath)
}

// loadOrCreateCA returns the persistent minting CA, creating it on first use.
// When a Secret source is configured (clientset + secretName), the CA is
// shared across pods via that Secret; otherwise it persists locally under
// caPath exactly as before.
func (p *EmbeddedProvider) loadOrCreateCA() (*MintingCA, error) {
	if err := os.MkdirAll(p.caPath, 0o700); err != nil {
		return nil, fmt.Errorf("create CA path: %w", err)
	}
	// Tighten an existing directory (e.g. a mounted PVC) so the cached CA key
	// is not readable by other users (CWE-732). Best-effort: when the pod runs
	// non-root and the directory is owned by another UID (e.g. created by an
	// earlier root-run deployment), chmod fails with EPERM. That is tolerable —
	// the CA key is still written 0600 below — so proceed rather than crash.
	if err := os.Chmod(p.caPath, 0o700); err != nil && !os.IsPermission(err) {
		return nil, fmt.Errorf("chmod CA path: %w", err)
	}

	if p.clientset != nil && p.secretName != "" {
		return p.loadOrCreateCAFromSecret()
	}

	caCertPath := filepath.Join(p.caPath, "ca.crt")
	caKeyPath := filepath.Join(p.caPath, "ca.key")

	if fileExists(caCertPath) && fileExists(caKeyPath) {
		return p.loadCA(caCertPath, caKeyPath)
	}

	return p.createCA(caCertPath, caKeyPath)
}

// loadOrCreateCAFromSecret shares the minting CA via a K8s Secret. The
// bootstrap node (ordinal 0 / caBootstrap) generates + writes it; other nodes
// poll until it appears. This mirrors ensureRaftCAFromSecret in raft_tls.go.
//
// A Secret that exists but has empty ca.crt/ca.key (e.g. pre-created by an
// older Helm chart revision with empty values) is treated as "not yet
// provisioned": the bootstrap node generates a CA and Updates the Secret
// rather than crashing on a PEM-decode error (CWE-754 / fail-closed).
func (p *EmbeddedProvider) loadOrCreateCAFromSecret() (*MintingCA, error) {
	if ca, err := p.readMintingCASecret(); err == nil {
		return ca, nil
	} else if !apierrors.IsNotFound(err) && !errors.Is(err, errEmptyMintingCASecret) {
		return nil, fmt.Errorf("read minting CA secret %s: %w", p.secretName, err)
	}

	if p.caBootstrap {
		certPEM, keyPEM, err := newMintingCAWithGoca()
		if err != nil {
			return nil, err
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: p.secretName, Namespace: p.namespace},
			Data: map[string][]byte{
				"ca.crt": certPEM,
				"ca.key": keyPEM,
			},
		}
		if _, err := p.clientset.CoreV1().Secrets(p.namespace).Create(context.Background(), secret, metav1.CreateOptions{}); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return nil, fmt.Errorf("create minting CA secret %s: %w", p.secretName, err)
			}
			// The Secret already exists. If it holds a valid CA (e.g. the
			// bootstrap pod restarted after a successful first boot), reuse it
			// so every pod keeps trusting the same minting CA across restarts.
			// If it is empty/invalid (e.g. pre-created by an older Helm chart),
			// overwrite it with the freshly generated CA.
			if ca, readErr := p.readMintingCASecret(); readErr == nil {
				return ca, nil
			} else if !errors.Is(readErr, errEmptyMintingCASecret) {
				return nil, fmt.Errorf("read existing minting CA secret %s: %w", p.secretName, readErr)
			}
			if _, err := p.clientset.CoreV1().Secrets(p.namespace).Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("update empty minting CA secret %s: %w", p.secretName, err)
			}
		}
		if err := p.writeLocalCA(certPEM, keyPEM); err != nil {
			return nil, err
		}
		return NewMintingCAFromPEM(certPEM, keyPEM, p.clientCertTTL)
	}

	// Non-bootstrap node: poll the Secret until the bootstrap pod writes it.
	timeout := p.secretPollTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		if ca, err := p.readMintingCASecret(); err == nil {
			return ca, nil
		} else if !apierrors.IsNotFound(err) && !errors.Is(err, errEmptyMintingCASecret) {
			return nil, fmt.Errorf("read minting CA secret %s: %w", p.secretName, err)
		} else if time.Now().After(deadline) {
			return nil, fmt.Errorf("minting CA secret %s did not appear within %s: %w", p.secretName, timeout, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// errEmptyMintingCASecret is returned by readMintingCASecret when the Secret
// exists but its ca.crt/ca.key values are empty (e.g. pre-created by an older
// Helm chart revision). The caller treats it like NotFound so the bootstrap
// node generates a CA and Updates the Secret instead of crashing.
var errEmptyMintingCASecret = errors.New("minting CA secret exists but ca.crt/ca.key are empty")

// readMintingCASecret fetches the CA cert+key from the shared K8s Secret,
// validates both keys, caches them locally, and returns the loaded MintingCA.
func (p *EmbeddedProvider) readMintingCASecret() (*MintingCA, error) {
	secret, err := p.clientset.CoreV1().Secrets(p.namespace).Get(context.Background(), p.secretName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	certPEM, okCert := secret.Data["ca.crt"]
	keyPEM, okKey := secret.Data["ca.key"]
	if !okCert || !okKey {
		return nil, fmt.Errorf("minting CA secret %s is missing ca.crt/ca.key keys", p.secretName)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, errEmptyMintingCASecret
	}
	if err := p.writeLocalCA(certPEM, keyPEM); err != nil {
		return nil, err
	}
	return NewMintingCAFromPEM(certPEM, keyPEM, p.clientCertTTL)
}

// writeLocalCA persists the CA cert+key under caPath (0600) as a local cache.
func (p *EmbeddedProvider) writeLocalCA(certPEM, keyPEM []byte) error {
	if err := os.WriteFile(filepath.Join(p.caPath, "ca.crt"), certPEM, 0o600); err != nil {
		return fmt.Errorf("write CA cert: %w", err)
	}
	if err := os.WriteFile(filepath.Join(p.caPath, "ca.key"), keyPEM, 0o600); err != nil {
		return fmt.Errorf("write CA key: %w", err)
	}
	return nil
}

// newMintingCAWithGoca builds a new self-signed minting CA via goca and
// returns PEM cert+key.
func newMintingCAWithGoca() (certPEM, keyPEM []byte, err error) {
	identity := goca.Identity{
		Organization:       "dagger-kubernetes",
		OrganizationalUnit: "engineering",
		Country:            "US",
		Locality:           "San Francisco",
		Province:           "California",
	}

	caInstance, err := goca.New("dagger-kubernetes-minting-ca", identity)
	if err != nil {
		return nil, nil, fmt.Errorf("create goca CA: %w", err)
	}
	return []byte(caInstance.GetCertificate()), []byte(caInstance.GetPrivateKey()), nil
}

func (p *EmbeddedProvider) createCA(certPath, keyPath string) (*MintingCA, error) {
	certPEM, keyPEM, err := newMintingCAWithGoca()
	if err != nil {
		return nil, err
	}

	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return nil, fmt.Errorf("write CA cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("write CA key: %w", err)
	}

	return p.loadCA(certPath, keyPath)
}

func (p *EmbeddedProvider) loadCA(certPath, keyPath string) (*MintingCA, error) {
	certPEM, err := os.ReadFile(certPath) //nolint:gosec // paths from config
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath) //nolint:gosec // paths from config
	if err != nil {
		return nil, fmt.Errorf("read CA key: %w", err)
	}

	return NewMintingCAFromPEM(certPEM, keyPEM, p.clientCertTTL)
}

func (p *EmbeddedProvider) issueServerCert(ca *MintingCA, certPath, keyPath string) (tls.Certificate, error) {
	sans := []string{"localhost", "supervisor", "supervisor-control", "supervisor-control.dagger-kubernetes.svc"}
	sans = append(sans, p.extraSANs...)
	// Direct pod access (CWE-295): cover this pod's hostname and loopback so
	// the data-plane cert verifies when dialed directly by pod name/IP. The
	// engine-facing data host is already included via extraSANs.
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		sans = append(sans, hostname)
	}
	sans = dedupeStrings(sans)
	// IssuePeerCertificate supports IP SANs (IssueServerCertificate does not),
	// so 127.0.0.1 is carried as an IP SAN. It also sets ExtKeyUsageServerAuth,
	// so the cert remains a valid server cert.
	certPEM, keyPEM, err := ca.IssuePeerCertificate(
		"supervisor-server", "dagger-kubernetes",
		sans,
		[]net.IP{net.ParseIP("127.0.0.1")},
		5*365*24*time.Hour)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("issue server cert: %w", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("write server cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("write server key: %w", err)
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// fileCAProvider serves the server TLS certificate from PEM files managed
// outside of dagger-kubernetes (cert-manager or external tooling). The minting CA
// is always auto-bootstrapped by the embedded provider (K8s Secret or local
// files), independent of where the server certificate comes from — the
// minting CA signs short-lived engine client certs and is an internal CA that
// never needs a public/cert-manager issuer.
type fileCAProvider struct {
	label    string
	certPath string
	keyPath  string
	minting  *EmbeddedProvider
}

var _ domain.CAProvider = (*fileCAProvider)(nil)

func (p *fileCAProvider) MintingCA() (domain.MintingCA, error) {
	return p.minting.MintingCA()
}

func (p *fileCAProvider) ServerTLSCert() (tls.Certificate, error) {
	return loadTLSKeyPair(p.certPath, p.keyPath)
}

// NewCertManagerProvider returns a CA provider backed by cert-manager
// mounted PEM files for the server certificate. The minting CA is
// auto-bootstrapped via the supplied embedded provider.
func NewCertManagerProvider(certPath, keyPath string, minting *EmbeddedProvider) domain.CAProvider {
	return &fileCAProvider{label: "cert-manager", certPath: certPath, keyPath: keyPath, minting: minting}
}

// NewExternalProvider returns a CA provider backed by externally managed PEM
// files for the server certificate. The minting CA is auto-bootstrapped via
// the supplied embedded provider.
func NewExternalProvider(certPath, keyPath string, minting *EmbeddedProvider) domain.CAProvider {
	return &fileCAProvider{label: "external", certPath: certPath, keyPath: keyPath, minting: minting}
}

// loadTLSKeyPair reads a PEM certificate/key pair and returns the TLS
// certificate.
func loadTLSKeyPair(certPath, keyPath string) (tls.Certificate, error) {
	certPEM, err := os.ReadFile(certPath) //nolint:gosec // paths from config
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read server cert: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath) //nolint:gosec // paths from config
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read server key: %w", err)
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}

// dedupeStrings returns in with duplicates removed, preserving order.
func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
