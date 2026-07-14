package ca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

type SerializableCertificate struct {
	CertificateChain [][]byte `json:"certificate_chain"` // DER-encoded
	PrivateKey       []byte   `json:"private_key"`       // PKCS8 DER
}

type MintingCA struct {
	cert    *x509.Certificate
	key     crypto.Signer
	certDER []byte

	clientCertTTL time.Duration
}

func NewMintingCA(clientCertTTL time.Duration) (*MintingCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:   "dagger-cache-minting-ca",
			Organization: []string{"dagger-cache"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        false,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA cert: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}

	return &MintingCA{
		cert:          cert,
		key:           key,
		certDER:       certDER,
		clientCertTTL: clientCertTTL,
	}, nil
}

func NewMintingCAFromPEM(certPEM, keyPEM []byte, clientCertTTL time.Duration) (*MintingCA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("failed to decode CA cert PEM")
	}

	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("failed to decode CA key PEM")
	}

	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %w", err)
	}

	return &MintingCA{
		cert:          cert,
		key:           key,
		certDER:       certBlock.Bytes,
		clientCertTTL: clientCertTTL,
	}, nil
}

func (ca *MintingCA) MintClientCert(commonName string) (*SerializableCertificate, error) {
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate client key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: commonName,
		},
		NotBefore: now,
		NotAfter:  now.Add(ca.clientCertTTL),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
		},
	}

	clientCertDER, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &clientKey.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("create client cert: %w", err)
	}

	clientKeyDER, err := x509.MarshalPKCS8PrivateKey(clientKey)
	if err != nil {
		return nil, fmt.Errorf("marshal client key: %w", err)
	}

	return &SerializableCertificate{
		CertificateChain: [][]byte{clientCertDER},
		PrivateKey:       clientKeyDER,
	}, nil
}

func (ca *MintingCA) CertPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}

func (ca *MintingCA) TLSCertificate() (tls.Certificate, error) {
	return tls.Certificate{
		Certificate: [][]byte{ca.certDER},
		PrivateKey:  ca.key,
	}, nil
}

func (ca *MintingCA) EncodePEM() (certPEM, keyPEM []byte, err error) {
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.certDER})

	keyDER, err := x509.MarshalECPrivateKey(ca.key.(*ecdsa.PrivateKey))
	if err != nil {
		return nil, nil, fmt.Errorf("marshal CA key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return certPEM, keyPEM, nil
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
		return tls.Certificate{}, fmt.Errorf("parse private key: %w", err)
	}

	return tls.Certificate{
		Certificate: sc.CertificateChain,
		PrivateKey:  key,
	}, nil
}
