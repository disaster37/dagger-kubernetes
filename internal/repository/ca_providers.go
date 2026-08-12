package repository

import (
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/disaster37/goca"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type EmbeddedProvider struct {
	caPath        string
	clientCertTTL time.Duration
	extraSANs     []string
}

var _ domain.CAProvider = (*EmbeddedProvider)(nil)

func NewEmbeddedProvider(caPath string, clientCertTTL time.Duration, extraSANs ...string) *EmbeddedProvider {
	return &EmbeddedProvider{
		caPath:        caPath,
		clientCertTTL: clientCertTTL,
		extraSANs:     extraSANs,
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
func (p *EmbeddedProvider) loadOrCreateCA() (*MintingCA, error) {
	if err := os.MkdirAll(p.caPath, 0o700); err != nil {
		return nil, fmt.Errorf("create CA path: %w", err)
	}

	caCertPath := filepath.Join(p.caPath, "ca.crt")
	caKeyPath := filepath.Join(p.caPath, "ca.key")

	if fileExists(caCertPath) && fileExists(caKeyPath) {
		return p.loadCA(caCertPath, caKeyPath)
	}

	return p.createCA(caCertPath, caKeyPath)
}

func (p *EmbeddedProvider) createCA(certPath, keyPath string) (*MintingCA, error) {
	identity := goca.Identity{
		Organization:       "dagger-cache",
		OrganizationalUnit: "engineering",
		Country:            "US",
		Locality:           "San Francisco",
		Province:           "California",
	}

	caInstance, err := goca.New("dagger-cache-minting-ca", identity)
	if err != nil {
		return nil, fmt.Errorf("create goca CA: %w", err)
	}

	if err := os.WriteFile(certPath, []byte(caInstance.GetCertificate()), 0o600); err != nil {
		return nil, fmt.Errorf("write CA cert: %w", err)
	}
	if err := os.WriteFile(keyPath, []byte(caInstance.GetPrivateKey()), 0o600); err != nil {
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
	sans := []string{"localhost", "supervisor", "supervisor-control", "supervisor-control.dagger-cache.svc"}
	sans = append(sans, p.extraSANs...)
	certPEM, keyPEM, err := ca.IssueServerCertificate(
		"supervisor-server", "dagger-cache",
		sans,
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

// fileCAProvider serves a minting CA and server TLS certificate from PEM
// files managed outside of dagger-cache (cert-manager or external tooling).
// The minting CA is loaded from the embedded CA path; the server certificate
// is loaded from the cert-manager or external PEM files.
type fileCAProvider struct {
	label    string
	certPath string
	keyPath  string
	caPath   string
}

var _ domain.CAProvider = (*fileCAProvider)(nil)

func (p *fileCAProvider) MintingCA() (domain.MintingCA, error) {
	caCertPath := filepath.Join(p.caPath, "ca.crt")
	caKeyPath := filepath.Join(p.caPath, "ca.key")
	certPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert from %s: %w", caCertPath, err)
	}
	keyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read CA key from %s: %w", caKeyPath, err)
	}
	return NewMintingCAFromPEM(certPEM, keyPEM, 2*time.Hour)
}

func (p *fileCAProvider) ServerTLSCert() (tls.Certificate, error) {
	return loadTLSKeyPair(p.certPath, p.keyPath)
}

// NewCertManagerProvider returns a CA provider backed by cert-manager
// mounted PEM files for the server certificate. The minting CA is loaded
// from the embedded CA path.
func NewCertManagerProvider(certPath, keyPath, caPath string) domain.CAProvider {
	return &fileCAProvider{label: "cert-manager", certPath: certPath, keyPath: keyPath, caPath: caPath}
}

// NewExternalProvider returns a CA provider backed by externally managed PEM
// files for the server certificate. The minting CA is loaded from the
// embedded CA path.
func NewExternalProvider(certPath, keyPath, caPath string) domain.CAProvider {
	return &fileCAProvider{label: "external", certPath: certPath, keyPath: keyPath, caPath: caPath}
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
