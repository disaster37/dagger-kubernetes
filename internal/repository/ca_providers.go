package repository

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	raftk8s "github.com/disaster37/go-raft/k8s"
	"github.com/disaster37/go-raft/pki"
	"k8s.io/client-go/kubernetes"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// mintingCACommonName / mintingCAOrganization identify the auto-bootstrapped
// minting CA (shared across pods via ca.minting_ca_secret).
const (
	mintingCACommonName   = "dagger-kubernetes-minting-ca"
	mintingCAOrganization = "dagger-kubernetes"
	mintingCACertValidity = 10 * 365 * 24 * time.Hour
	serverCertValidity    = 5 * 365 * 24 * time.Hour
)

// EmbeddedProvider serves the data-plane server certificate from a leaf minted
// by the auto-bootstrapped minting CA. The CA bootstrap (Kubernetes Secret or
// local files) and the leaf reuse/expiry checks are delegated to go-raft's
// k8s/pki packages.
type EmbeddedProvider struct {
	caPath        string
	clientCertTTL time.Duration
	extraSANs     []string
	bootstrap     pki.BootstrapProvider
	safetyMargin  time.Duration
}

var _ domain.CAProvider = (*EmbeddedProvider)(nil)

// NewEmbeddedProvider returns a local-file minting-CA provider (single-node
// dev/test or a shared RWX volume).
func NewEmbeddedProvider(caPath string, clientCertTTL time.Duration, extraSANs ...string) *EmbeddedProvider {
	return &EmbeddedProvider{
		caPath:        caPath,
		clientCertTTL: clientCertTTL,
		extraSANs:     extraSANs,
		bootstrap:     pki.NewLocalBootstrapProvider(caPath, mintingCACommonName, mintingCAOrganization, mintingCACertValidity),
		safetyMargin:  pki.DefaultSafetyMargin,
	}
}

// NewEmbeddedProviderWithSecret returns an EmbeddedProvider that shares the
// minting CA across pods via a Kubernetes Secret (ca.minting_ca_secret). A nil
// clientset or empty secret name falls back to local files.
func NewEmbeddedProviderWithSecret(caPath string, clientCertTTL time.Duration, secretName, namespace string, clientset kubernetes.Interface, caBootstrap bool, secretPollTimeout time.Duration, extraSANs ...string) *EmbeddedProvider {
	p := &EmbeddedProvider{
		caPath:        caPath,
		clientCertTTL: clientCertTTL,
		extraSANs:     extraSANs,
		safetyMargin:  pki.DefaultSafetyMargin,
	}
	if clientset == nil || secretName == "" {
		p.bootstrap = pki.NewLocalBootstrapProvider(caPath, mintingCACommonName, mintingCAOrganization, mintingCACertValidity)
		return p
	}
	p.bootstrap = raftk8s.NewSecretBootstrapProvider(raftk8s.SecretBootstrapConfig{
		Clientset:    clientset,
		Namespace:    namespace,
		SecretName:   secretName,
		CABootstrap:  caBootstrap,
		CACommonName: mintingCACommonName,
		Organization: mintingCAOrganization,
		CAValidity:   mintingCACertValidity,
		PollTimeout:  secretPollTimeout,
		CacheDir:     caPath,
	})
	return p
}

// MintingCA returns the persistent minting CA, creating it on first use.
func (p *EmbeddedProvider) MintingCA() (domain.MintingCA, error) {
	return p.loadOrCreateCA()
}

// BootstrapProvider returns the underlying CA bootstrap provider (used by the
// MintingRotator to seed the replicated keyring).
func (p *EmbeddedProvider) BootstrapProvider() pki.BootstrapProvider {
	return p.bootstrap
}

// ServerTLSCert returns the data-plane server certificate, re-issuing it in
// place when the cached pair no longer covers the required SAN set or is near
// expiry.
func (p *EmbeddedProvider) ServerTLSCert() (tls.Certificate, error) {
	ca, err := p.loadOrCreateCA()
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("minting CA: %w", err)
	}
	sans, ipSANs := p.requiredServerSANs()
	return p.issueOrReuseServerCert(ca, "server.crt", "server.key", "supervisor-server", sans, ipSANs)
}

// InternalServerTLSCert returns the leaf served by the internal-only control
// listener (ADR-041). It is minted by the shared minting CA in every provider
// mode and carries this pod's own exact FQDN SANs (no wildcard).
func (p *EmbeddedProvider) InternalServerTLSCert() (tls.Certificate, error) {
	ca, err := p.loadOrCreateCA()
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("minting CA: %w", err)
	}
	return p.issueOrReuseServerCert(ca, "server-internal.crt", "server-internal.key", "supervisor-internal", p.internalServerSANs(), nil)
}

// internalServerSANs returns the exact SAN set the internal-listener leaf must
// cover: this pod's own FQDN SANs plus the engine-facing data host, already
// computed by mintingProvider into extraSANs.
func (p *EmbeddedProvider) internalServerSANs() []string {
	return dedupeStrings(p.extraSANs)
}

// requiredServerSANs returns the exact DNS + IP SAN set the embedded server
// certificate must cover.
func (p *EmbeddedProvider) requiredServerSANs() (dns []string, ips []net.IP) {
	sans := []string{"localhost", "supervisor", "supervisor-control", "supervisor-control.dagger-kubernetes.svc"}
	sans = append(sans, p.extraSANs...)
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		sans = append(sans, hostname)
	}
	return dedupeStrings(sans), []net.IP{net.ParseIP("127.0.0.1")}
}

// loadOrCreateCA returns the persistent minting CA via the configured
// bootstrap provider (Kubernetes Secret or local files).
func (p *EmbeddedProvider) loadOrCreateCA() (*MintingCA, error) {
	mat, err := p.bootstrap.EnsureBootstrapMaterial(context.Background())
	if err != nil {
		return nil, err
	}
	return NewMintingCAFromPEM(mat.CACertPEM, mat.CAKeyPEM, p.clientCertTTL)
}

// issueOrReuseServerCert reuses the cached leaf when it is still valid, signed
// by the current CA, has the expected CN, and covers the required SANs;
// otherwise it re-issues and persists the pair (0600).
func (p *EmbeddedProvider) issueOrReuseServerCert(ca *MintingCA, certFile, keyFile, commonName string, dnsNames []string, ipAddrs []net.IP) (tls.Certificate, error) {
	certPath := filepath.Join(p.caPath, certFile)
	keyPath := filepath.Join(p.caPath, keyFile)

	if certPEM, keyPEM, ok := p.reuseServerCert(ca, certPath, keyPath, commonName, dnsNames, ipAddrs); ok {
		return tls.X509KeyPair(certPEM, keyPEM)
	}

	certPEM, keyPEM, err := ca.IssuePeerCertificate(commonName, mintingCAOrganization, dnsNames, ipAddrs, serverCertValidity)
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

// reuseServerCert reports whether the cached pair may be reused, returning its
// PEM bytes when so.
func (p *EmbeddedProvider) reuseServerCert(ca *MintingCA, certPath, keyPath, commonName string, dnsNames []string, ipAddrs []net.IP) (certPEM, keyPEM []byte, ok bool) {
	certPEM, err := os.ReadFile(certPath) //nolint:gosec // fixed file under the configured CA dir
	if err != nil {
		return nil, nil, false
	}
	keyPEM, err = os.ReadFile(keyPath) //nolint:gosec // fixed file under the configured CA dir
	if err != nil {
		return nil, nil, false
	}
	cert, err := pki.ParsePEMCert(certPEM)
	if err != nil {
		return nil, nil, false
	}
	if !pki.ReusableNodeCert(cert, ca.ca, commonName, dnsNames, ipAddrs, p.safetyMargin, time.Now()) {
		return nil, nil, false
	}
	return certPEM, keyPEM, true
}

// fileCAProvider serves the server TLS certificate from PEM files managed
// outside of dagger-kubernetes (cert-manager or external tooling). The minting
// CA is always auto-bootstrapped by the embedded provider.
type fileCAProvider struct {
	label    string
	certPath string
	keyPath  string
	minting  domain.CAProvider
}

var _ domain.CAProvider = (*fileCAProvider)(nil)

func (p *fileCAProvider) MintingCA() (domain.MintingCA, error) {
	return p.minting.MintingCA()
}

func (p *fileCAProvider) ServerTLSCert() (tls.Certificate, error) {
	return loadTLSKeyPair(p.certPath, p.keyPath)
}

// InternalServerTLSCert delegates to the embedded minting provider so the
// internal listener always serves a minting-CA leaf.
func (p *fileCAProvider) InternalServerTLSCert() (tls.Certificate, error) {
	return p.minting.InternalServerTLSCert()
}

// NewCertManagerProvider returns a CA provider backed by cert-manager mounted
// PEM files for the server certificate.
func NewCertManagerProvider(certPath, keyPath string, minting domain.CAProvider) domain.CAProvider {
	return &fileCAProvider{label: "cert-manager", certPath: certPath, keyPath: keyPath, minting: minting}
}

// NewExternalProvider returns a CA provider backed by externally managed PEM
// files for the server certificate.
func NewExternalProvider(certPath, keyPath string, minting domain.CAProvider) domain.CAProvider {
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
