package repository

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"time"

	"github.com/disaster37/goca"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// MintingCA wraps a goca CA for engine/client certificate issuance and
// TLS server/peer certificate issuance. All key generation and certificate
// signing is delegated to goca.
type MintingCA struct {
	gocaCA        *goca.CA
	clientCertTTL time.Duration
}

var _ domain.MintingCA = (*MintingCA)(nil)

// NewMintingCA creates a new self-signed minting CA via goca. This is the
// non-Kubernetes fallback path. Production Kubernetes deployments use the
// auto-bootstrapped CA from ca_providers.go (shared via Secret).
func NewMintingCA(clientCertTTL time.Duration) (*MintingCA, error) {
	ca, err := goca.New("dagger-kubernetes-minting-ca", goca.Identity{
		Organization:       "dagger-kubernetes",
		OrganizationalUnit: "engineering",
		Country:            "US",
		Locality:           "San Francisco",
		Province:           "California",
		Valid:              3650, // 10 years
	})
	if err != nil {
		return nil, fmt.Errorf("create minting CA: %w", err)
	}
	return &MintingCA{gocaCA: ca, clientCertTTL: clientCertTTL}, nil
}

// NewMintingCAFromPEM loads an existing CA from PEM-encoded certificate and
// private key. Uses goca's LoadCAFromPEM which derives the public key and
// synthesizes an empty CRL.
func NewMintingCAFromPEM(certPEM, keyPEM []byte, clientCertTTL time.Duration) (*MintingCA, error) {
	ca := &goca.CA{}
	if err := ca.LoadCAFromPEM(certPEM, keyPEM); err != nil {
		fixedKey, fixErr := fixPKCS1KeyWithWrongHeader(keyPEM)
		if fixErr != nil {
			return nil, fmt.Errorf("load minting CA from PEM: %w", err)
		}
		if err2 := ca.LoadCAFromPEM(certPEM, fixedKey); err2 != nil {
			return nil, fmt.Errorf("load minting CA from PEM: %w", err)
		}
	}
	return &MintingCA{gocaCA: ca, clientCertTTL: clientCertTTL}, nil
}

func fixPKCS1KeyWithWrongHeader(keyPEM []byte) ([]byte, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("decode key PEM")
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("key PEM type is %q, not PKCS#8", block.Type)
	}
	if _, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return nil, fmt.Errorf("key is valid PKCS#8")
	}
	if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
		return nil, fmt.Errorf("key is neither PKCS#1 nor PKCS#8: %w", err)
	}
	fixed := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: block.Bytes})
	return fixed, nil
}

// MintClientCert issues a short-lived engine client certificate (client
// auth only). The TTL is ca.clientCertTTL (typically 2h).
func (ca *MintingCA) MintClientCert(commonName string) (*domain.SerializableCertificate, error) {
	cert, err := ca.gocaCA.IssueCertificate(commonName, goca.Identity{
		Organization:       "dagger-kubernetes",
		OrganizationalUnit: "engine",
		Country:            "US",
		Locality:           "San Francisco",
		Province:           "California",
		Type:               "client",
		ValidDuration:      ca.clientCertTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("mint engine client cert: %w", err)
	}

	// Extract DER: cert PEM → DER, key PEM → DER.
	certBlock, _ := pem.Decode([]byte(cert.Certificate))
	if certBlock == nil {
		return nil, fmt.Errorf("decode issued cert PEM")
	}
	keyBlock, _ := pem.Decode([]byte(cert.PrivateKey))
	if keyBlock == nil {
		return nil, fmt.Errorf("decode issued key PEM")
	}

	return &domain.SerializableCertificate{
		CertificateChain: [][]byte{certBlock.Bytes},
		PrivateKey:       keyBlock.Bytes,
	}, nil
}

// CertPool returns a CertPool containing this CA's certificate.
func (ca *MintingCA) CertPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.gocaCA.GoCertificate())
	return pool
}

// CACertificate returns the parsed CA certificate.
func (ca *MintingCA) CACertificate() *x509.Certificate {
	return ca.gocaCA.GoCertificate()
}

// TLSCertificate returns the CA certificate as a tls.Certificate.
func (ca *MintingCA) TLSCertificate() (tls.Certificate, error) {
	// goca stores the certificate as PEM string; decode to DER.
	certBlock, _ := pem.Decode([]byte(ca.gocaCA.GetCertificate()))
	if certBlock == nil {
		return tls.Certificate{}, fmt.Errorf("decode CA cert PEM")
	}
	return tls.Certificate{
		Certificate: [][]byte{certBlock.Bytes},
		PrivateKey:  ca.gocaCA.GoSigner(),
	}, nil
}

// EncodePEM returns the CA certificate and private key as PEM bytes (for
// Kubernetes Secret storage). Format: "CERTIFICATE" + "PRIVATE KEY" (PKCS#8).
func (ca *MintingCA) EncodePEM() (certPEM, keyPEM []byte, err error) {
	return []byte(ca.gocaCA.GetCertificate()), []byte(ca.gocaCA.GetPrivateKey()), nil
}

// IssueServerCertificate signs a TLS server certificate with DNS SANs.
func (ca *MintingCA) IssueServerCertificate(commonName, organization string, dnsNames []string, ttl time.Duration) (certPEM, keyPEM []byte, err error) {
	days := int(ttl.Hours() / 24)
	if days < 1 {
		days = 1
	}
	cert, err := ca.gocaCA.IssueCertificate(commonName, goca.Identity{
		Organization:       organization,
		OrganizationalUnit: "engineering",
		Country:            "US",
		Locality:           "San Francisco",
		Province:           "California",
		Type:               "server",
		Valid:              days,
		DNSNames:           dnsNames,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("issue server cert: %w", err)
	}
	return []byte(cert.Certificate), []byte(cert.PrivateKey), nil
}

// IssuePeerCertificate signs a TLS mTLS peer certificate with DNS + IP SANs.
// The certificate is backdated 5 minutes for clock-skew tolerance (ADR-016).
func (ca *MintingCA) IssuePeerCertificate(commonName, organization string, dnsNames []string, ipAddrs []net.IP, ttl time.Duration) (certPEM, keyPEM []byte, err error) {
	cert, err := ca.gocaCA.IssueCertificate(commonName, goca.Identity{
		Organization:       organization,
		OrganizationalUnit: "engineering",
		Country:            "US",
		Locality:           "San Francisco",
		Province:           "California",
		Type:               "server-client",
		ValidDuration:      ttl,
		Backdate:           5 * time.Minute,
		DNSNames:           dnsNames,
		IPAddresses:        ipAddrs,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("issue peer cert: %w", err)
	}
	return []byte(cert.Certificate), []byte(cert.PrivateKey), nil
}
