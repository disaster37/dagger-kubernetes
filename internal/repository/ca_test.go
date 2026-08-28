package repository

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
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
	if block == nil || block.Type != "PRIVATE KEY" {
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

func TestMintingCAIssuePeerCertificate(t *testing.T) {
	ca, err := NewMintingCA(1 * time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCA: %v", err)
	}

	dnsNames := []string{"localhost", "node-0.headless.ns.svc.cluster.local"}
	ipAddrs := []net.IP{net.ParseIP("127.0.0.1")}
	certPEM, keyPEM, err := ca.IssuePeerCertificate("node-0", "dagger-kubernetes-raft", dnsNames, ipAddrs, 24*time.Hour)
	if err != nil {
		t.Fatalf("IssuePeerCertificate: %v", err)
	}

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	if len(tlsCert.Certificate) == 0 {
		t.Fatal("certificate chain is empty")
	}

	cert, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	if cert.Subject.CommonName != "node-0" {
		t.Fatalf("CN = %q, want node-0", cert.Subject.CommonName)
	}
	if len(cert.DNSNames) < len(dnsNames) {
		t.Fatalf("DNSNames = %v, want at least %v", cert.DNSNames, dnsNames)
	}
	for i, n := range dnsNames {
		found := false
		for _, dns := range cert.DNSNames {
			if dns == n {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("DNSNames missing %q, got %v", n, cert.DNSNames)
		}
		_ = i
	}
	if len(cert.IPAddresses) != 1 || !cert.IPAddresses[0].Equal(ipAddrs[0]) {
		t.Fatalf("IPAddresses = %v, want %v", cert.IPAddresses, ipAddrs)
	}

	hasServer := false
	hasClient := false
	for _, usage := range cert.ExtKeyUsage {
		if usage == x509.ExtKeyUsageServerAuth {
			hasServer = true
		}
		if usage == x509.ExtKeyUsageClientAuth {
			hasClient = true
		}
	}
	if !hasServer || !hasClient {
		t.Fatalf("ExtKeyUsage = %v, want ServerAuth + ClientAuth", cert.ExtKeyUsage)
	}

	roots := ca.CertPool()
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("peer cert not trusted for server auth: %v", err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("peer cert not trusted for client auth: %v", err)
	}

	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("key PEM type = %s, want PRIVATE KEY", block.Type)
	}
}
