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
}

var _ domain.CAProvider = (*EmbeddedProvider)(nil)

func NewEmbeddedProvider(caPath string, clientCertTTL time.Duration) *EmbeddedProvider {
	return &EmbeddedProvider{
		caPath:        caPath,
		clientCertTTL: clientCertTTL,
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
	certPEM, keyPEM, err := ca.IssueServerCertificate(
		"supervisor-server", "dagger-cache",
		[]string{"localhost", "supervisor", "supervisor-control", "supervisor-control.dagger-cache.svc"},
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
type fileCAProvider struct {
	label    string
	certPath string
	keyPath  string
}

var _ domain.CAProvider = (*fileCAProvider)(nil)

func (p *fileCAProvider) MintingCA() (domain.MintingCA, error) {
	certPEM, keyPEM, err := p.readPEM()
	if err != nil {
		return nil, err
	}
	return NewMintingCAFromPEM(certPEM, keyPEM, 2*time.Hour)
}

func (p *fileCAProvider) ServerTLSCert() (tls.Certificate, error) {
	certPEM, keyPEM, err := p.readPEM()
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

func (p *fileCAProvider) readPEM() (certPEM, keyPEM []byte, err error) {
	certPEM, err = os.ReadFile(p.certPath) //nolint:gosec // paths from config
	if err != nil {
		return nil, nil, fmt.Errorf("read %s cert: %w", p.label, err)
	}
	keyPEM, err = os.ReadFile(p.keyPath) //nolint:gosec // paths from config
	if err != nil {
		return nil, nil, fmt.Errorf("read %s key: %w", p.label, err)
	}
	return certPEM, keyPEM, nil
}

// NewCertManagerProvider returns a CA provider backed by cert-manager
// mounted PEM files.
func NewCertManagerProvider(certPath, keyPath string) domain.CAProvider {
	return &fileCAProvider{label: "cert-manager", certPath: certPath, keyPath: keyPath}
}

// NewExternalProvider returns a CA provider backed by externally managed PEM
// files.
func NewExternalProvider(certPath, keyPath string) domain.CAProvider {
	return &fileCAProvider{label: "external", certPath: certPath, keyPath: keyPath}
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
