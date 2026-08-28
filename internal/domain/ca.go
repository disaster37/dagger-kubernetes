package domain

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
)

type SerializableCertificate struct {
	CertificateChain [][]byte `json:"certificate_chain"` // DER-encoded
	PrivateKey       []byte   `json:"private_key"`       // PKCS8 DER
}

type MintingCA interface {
	MintClientCert(commonName string) (*SerializableCertificate, error)
	CertPool() *x509.CertPool
}

type CAProvider interface {
	MintingCA() (MintingCA, error)
	ServerTLSCert() (tls.Certificate, error)
}

func (sc *SerializableCertificate) Fingerprint() string {
	if len(sc.CertificateChain) == 0 {
		return ""
	}
	cert, err := x509.ParseCertificate(sc.CertificateChain[0])
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", cert.SerialNumber)
}

func (sc *SerializableCertificate) TLSClientCertificate() (tls.Certificate, error) {
	if len(sc.CertificateChain) == 0 || len(sc.PrivateKey) == 0 {
		return tls.Certificate{}, fmt.Errorf("incomplete certificate")
	}

	key, err := x509.ParsePKCS8PrivateKey(sc.PrivateKey)
	if err != nil {
		rsaKey, rsaErr := x509.ParsePKCS1PrivateKey(sc.PrivateKey)
		if rsaErr != nil {
			return tls.Certificate{}, fmt.Errorf("parse private key: %w", err)
		}
		key = rsaKey
	}

	return tls.Certificate{
		Certificate: sc.CertificateChain,
		PrivateKey:  key,
	}, nil
}
