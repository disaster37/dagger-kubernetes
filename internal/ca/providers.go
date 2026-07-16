package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/disaster37/goca"
)

type EmbeddedProvider struct {
	caPath        string
	clientCertTTL time.Duration
}

func NewEmbeddedProvider(caPath string, clientCertTTL time.Duration) *EmbeddedProvider {
	return &EmbeddedProvider{
		caPath:        caPath,
		clientCertTTL: clientCertTTL,
	}
}

func (p *EmbeddedProvider) MintingCA() (*MintingCA, error) {
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

func (p *EmbeddedProvider) ServerTLSCert() (tls.Certificate, error) {
	ca, err := p.MintingCA()
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("minting CA: %w", err)
	}

	serverCertPath := filepath.Join(p.caPath, "server.crt")
	serverKeyPath := filepath.Join(p.caPath, "server.key")

	if fileExists(serverCertPath) && fileExists(serverKeyPath) {
		certPEM, err := os.ReadFile(serverCertPath) //nolint:gosec // paths from config
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("read server cert: %w", err)
		}
		keyPEM, err := os.ReadFile(serverKeyPath) //nolint:gosec // paths from config
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("read server key: %w", err)
		}
		return tls.X509KeyPair(certPEM, keyPEM)
	}

	return p.issueServerCert(ca, serverCertPath, serverKeyPath)
}

func (p *EmbeddedProvider) createCA(certPath, keyPath string) (*MintingCA, error) {
	identity := goca.Identity{
		Organization: "dagger-cache",
		Country:      "US",
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
	caCert := ca.cert

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate server key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "supervisor-server",
			Organization: []string{"dagger-cache"},
		},
		NotBefore: now,
		NotAfter:  now.Add(5 * 365 * 24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		DNSNames: []string{"localhost", "supervisor", "supervisor-control", "supervisor-control.dagger-cache.svc"},
	}

	caSigner := ca.key
	serverCertDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &serverKey.PublicKey, caSigner)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create server cert: %w", err)
	}

	certPEMBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCertDER})
	keyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal server key: %w", err)
	}
	keyPEMBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(certPath, certPEMBytes, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("write server cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEMBytes, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("write server key: %w", err)
	}

	return tls.X509KeyPair(certPEMBytes, keyPEMBytes)
}

type CertManagerProvider struct {
	certPath string
	keyPath  string
}

func NewCertManagerProvider(certPath, keyPath string) *CertManagerProvider {
	return &CertManagerProvider{certPath: certPath, keyPath: keyPath}
}

func (p *CertManagerProvider) MintingCA() (*MintingCA, error) {
	certPEM, err := os.ReadFile(p.certPath)
	if err != nil {
		return nil, fmt.Errorf("read cert-manager cert: %w", err)
	}
	keyPEM, err := os.ReadFile(p.keyPath)
	if err != nil {
		return nil, fmt.Errorf("read cert-manager key: %w", err)
	}
	return NewMintingCAFromPEM(certPEM, keyPEM, 2*time.Hour)
}

func (p *CertManagerProvider) ServerTLSCert() (tls.Certificate, error) {
	certPEM, err := os.ReadFile(p.certPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read cert-manager cert: %w", err)
	}
	keyPEM, err := os.ReadFile(p.keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read cert-manager key: %w", err)
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

type ExternalProvider struct {
	certPath string
	keyPath  string
}

func NewExternalProvider(certPath, keyPath string) *ExternalProvider {
	return &ExternalProvider{certPath: certPath, keyPath: keyPath}
}

func (p *ExternalProvider) MintingCA() (*MintingCA, error) {
	certPEM, err := os.ReadFile(p.certPath)
	if err != nil {
		return nil, fmt.Errorf("read external CA cert: %w", err)
	}
	keyPEM, err := os.ReadFile(p.keyPath)
	if err != nil {
		return nil, fmt.Errorf("read external CA key: %w", err)
	}
	return NewMintingCAFromPEM(certPEM, keyPEM, 2*time.Hour)
}

func (p *ExternalProvider) ServerTLSCert() (tls.Certificate, error) {
	certPEM, err := os.ReadFile(p.certPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read external cert: %w", err)
	}
	keyPEM, err := os.ReadFile(p.keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read external key: %w", err)
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}
