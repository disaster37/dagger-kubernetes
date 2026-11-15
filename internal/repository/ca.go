package repository

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"time"

	"github.com/disaster37/go-raft/pki"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// MintingCA is a thin adapter over go-raft's pki.MintingCA. It retains no
// crypto/goca logic: all key generation and certificate signing is delegated
// to the shared library. It exists only to expose the supervisor's
// domain.MintingCA interface (which returns DER slices, because
// internal/domain is stdlib-only per ADR-009).
type MintingCA struct {
	ca            *pki.MintingCA
	clientCertTTL time.Duration
}

var _ domain.MintingCA = (*MintingCA)(nil)

// NewMintingCA creates a new self-signed minting CA. This is the
// non-Kubernetes fallback path. Production Kubernetes deployments use the
// auto-bootstrapped CA from ca_providers.go (shared via Secret).
func NewMintingCA(clientCertTTL time.Duration) (*MintingCA, error) {
	ca, err := pki.NewMintingCA("dagger-kubernetes-minting-ca", "dagger-kubernetes", 10*365*24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("create minting CA: %w", err)
	}
	return &MintingCA{ca: ca, clientCertTTL: clientCertTTL}, nil
}

// NewMintingCAFromPEM loads an existing CA from PEM-encoded certificate and
// private key, repairing a PKCS#1 key stored under a PKCS#8 header.
func NewMintingCAFromPEM(certPEM, keyPEM []byte, clientCertTTL time.Duration) (*MintingCA, error) {
	ca, err := pki.NewMintingCAFromPEM(certPEM, keyPEM, clientCertTTL)
	if err != nil {
		return nil, fmt.Errorf("load minting CA from PEM: %w", err)
	}
	return &MintingCA{ca: ca, clientCertTTL: clientCertTTL}, nil
}

// MintClientCert issues a short-lived engine client certificate (client auth
// only). The TTL is ca.clientCertTTL (typically 2h).
func (ca *MintingCA) MintClientCert(commonName string) (*domain.SerializableCertificate, error) {
	certPEM, keyPEM, err := ca.ca.IssueClientCertificate(commonName, "dagger-kubernetes", ca.clientCertTTL)
	if err != nil {
		return nil, fmt.Errorf("mint engine client cert: %w", err)
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("decode issued cert PEM")
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("decode issued key PEM")
	}
	return &domain.SerializableCertificate{
		CertificateChain: [][]byte{certBlock.Bytes},
		PrivateKey:       keyBlock.Bytes,
	}, nil
}

// CertPool returns a CertPool containing this CA's certificate.
func (ca *MintingCA) CertPool() *x509.CertPool { return ca.ca.CertPool() }

// CACertificate returns the parsed CA certificate.
func (ca *MintingCA) CACertificate() *x509.Certificate { return ca.ca.CACertificate() }

// TLSCertificate returns the CA certificate as a tls.Certificate.
func (ca *MintingCA) TLSCertificate() (tls.Certificate, error) { return ca.ca.TLSCertificate() }

// EncodePEM returns the CA certificate and private key as PEM bytes (for
// Kubernetes Secret storage).
func (ca *MintingCA) EncodePEM() (certPEM, keyPEM []byte, err error) { return ca.ca.EncodePEM() }

// IssueServerCertificate signs a TLS server certificate with DNS SANs.
func (ca *MintingCA) IssueServerCertificate(commonName, organization string, dnsNames []string, ttl time.Duration) (certPEM, keyPEM []byte, err error) {
	return ca.ca.IssueServerCertificate(commonName, organization, dnsNames, ttl)
}

// IssuePeerCertificate signs a TLS mTLS peer certificate with DNS + IP SANs.
func (ca *MintingCA) IssuePeerCertificate(commonName, organization string, dnsNames []string, ipAddrs []net.IP, ttl time.Duration) (certPEM, keyPEM []byte, err error) {
	return ca.ca.IssuePeerCertificate(commonName, organization, dnsNames, ipAddrs, ttl)
}
