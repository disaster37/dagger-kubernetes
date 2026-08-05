package ca

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"
	"time"
)

func TestMintingCARoundTrip(t *testing.T) {
	ttl := 2 * time.Hour
	ca, err := NewMintingCA(ttl)
	if err != nil {
		t.Fatalf("NewMintingCA: %v", err)
	}

	sc, err := ca.MintClientCert("test-client-1")
	if err != nil {
		t.Fatalf("MintClientCert: %v", err)
	}

	if len(sc.CertificateChain) == 0 {
		t.Fatal("CertificateChain is empty")
	}
	if len(sc.PrivateKey) == 0 {
		t.Fatal("PrivateKey is empty")
	}

	fp := sc.Fingerprint()
	if fp == "" {
		t.Fatal("Fingerprint is empty")
	}

	clientCert, err := x509.ParseCertificate(sc.CertificateChain[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	if clientCert.Subject.CommonName != "test-client-1" {
		t.Fatalf("expected CN=test-client-1, got %s", clientCert.Subject.CommonName)
	}

	roots := ca.CertPool()
	opts := x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if _, err := clientCert.Verify(opts); err != nil {
		t.Fatalf("client cert not trusted by CA: %v", err)
	}
}

func TestMintingCAPEMRoundTrip(t *testing.T) {
	ca, err := NewMintingCA(1 * time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCA: %v", err)
	}

	certPEM, keyPEM, err := ca.EncodePEM()
	if err != nil {
		t.Fatalf("EncodePEM: %v", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("invalid cert PEM")
	}
	block, _ = pem.Decode(keyPEM)
	if block == nil || (block.Type != "PRIVATE KEY" && block.Type != "EC PRIVATE KEY") {
		t.Fatalf("invalid key PEM type: %s", block.Type)
	}

	ca2, err := NewMintingCAFromPEM(certPEM, keyPEM, 1*time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCAFromPEM: %v", err)
	}

	sc, err := ca2.MintClientCert("test-client-2")
	if err != nil {
		t.Fatalf("MintClientCert: %v", err)
	}

	clientCert, err := x509.ParseCertificate(sc.CertificateChain[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	roots := ca2.CertPool()
	opts := x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if _, err := clientCert.Verify(opts); err != nil {
		t.Fatalf("round-tripped client cert not trusted: %v", err)
	}
}

func TestSerializableCertificateToTLS(t *testing.T) {
	ca, _ := NewMintingCA(1 * time.Hour)
	sc, _ := ca.MintClientCert("test-tls")

	tlsCert, err := sc.TLSClientCertificate()
	if err != nil {
		t.Fatalf("TLSClientCertificate: %v", err)
	}
	if len(tlsCert.Certificate) == 0 {
		t.Fatal("TLS cert chain is empty")
	}
	if tlsCert.PrivateKey == nil {
		t.Fatal("TLS private key is nil")
	}
}

func TestEmbeddedProviderCreateCA(t *testing.T) {
	dir, err := os.MkdirTemp("", "ca-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)

	p := NewEmbeddedProvider(dir, 2*time.Hour)

	ca, err := p.MintingCA()
	if err != nil {
		t.Fatalf("MintingCA: %v", err)
	}

	sc, err := ca.MintClientCert("embedded-client")
	if err != nil {
		t.Fatalf("MintClientCert: %v", err)
	}

	clientCert, err := x509.ParseCertificate(sc.CertificateChain[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	if clientCert.Subject.CommonName != "embedded-client" {
		t.Fatalf("expected CN=embedded-client, got %s", clientCert.Subject.CommonName)
	}

	roots := ca.CertPool()
	opts := x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if _, err := clientCert.Verify(opts); err != nil {
		t.Fatalf("client cert not trusted: %v", err)
	}

	certPath := dir + "/ca.crt"
	keyPath := dir + "/ca.key"
	serverCertPath := dir + "/server.crt"
	serverKeyPath := dir + "/server.key"

	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		t.Fatal("ca.crt not created")
	}
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		t.Fatal("ca.key not created")
	}

	tlsCert, err := p.ServerTLSCert()
	if err != nil {
		t.Fatalf("ServerTLSCert: %v", err)
	}
	if len(tlsCert.Certificate) == 0 {
		t.Fatal("server cert chain is empty")
	}
	if tlsCert.PrivateKey == nil {
		t.Fatal("server private key is nil")
	}
	if _, err := os.Stat(serverCertPath); os.IsNotExist(err) {
		t.Fatal("server.crt not created")
	}
	if _, err := os.Stat(serverKeyPath); os.IsNotExist(err) {
		t.Fatal("server.key not created")
	}

	ca2, err := p.MintingCA()
	if err != nil {
		t.Fatalf("MintingCA second call: %v", err)
	}

	sc2, err := ca2.MintClientCert("embedded-client-2")
	if err != nil {
		t.Fatalf("MintClientCert from reloaded CA: %v", err)
	}
	clientCert2, err := x509.ParseCertificate(sc2.CertificateChain[0])
	if err != nil {
		t.Fatalf("ParseCertificate from reloaded CA: %v", err)
	}
	if _, err := clientCert2.Verify(opts); err != nil {
		t.Fatalf("reloaded CA client cert not trusted: %v", err)
	}
}
