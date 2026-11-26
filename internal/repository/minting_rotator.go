package repository

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	raftconfig "github.com/disaster37/go-raft/config"
	"github.com/disaster37/go-raft/keyring"
	raftobserv "github.com/disaster37/go-raft/observ"
	"github.com/disaster37/go-raft/pki"
	"github.com/disaster37/go-raft/storage"
	rafttransport "github.com/disaster37/go-raft/transport"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// mintingKeyringMetaKey is the replicated FSM meta key holding the Minting CA
// keyring JSON. Every pod observes it via the FSM onMeta hook.
const mintingKeyringMetaKey = "minting_tls_keyring"

// MintingKeyringMetaKey is the exported name of the replicated Minting CA
// keyring meta key.
const MintingKeyringMetaKey = mintingKeyringMetaKey

// DynamicMintingCA is the atomic, rotation-aware Minting CA + server-cert
// provider. It implements domain.CAProvider and domain.MintingCA. The active
// CA, the union trust pool, and the server/internal leaves are swapped
// atomically as the replicated keyring advances.
type DynamicMintingCA struct {
	provider *EmbeddedProvider
	sealer   storage.Sealer
	logger   *logrus.Logger

	state atomic.Pointer[dynamicMintingState]

	internalProvider *rafttransport.DynamicTLSProvider
	forwardProvider  *rafttransport.DynamicTLSProvider
	embeddedProvider *rafttransport.DynamicTLSProvider

	mu sync.Mutex
}

type dynamicMintingState struct {
	active       *MintingCA
	pool         *x509.CertPool
	keyring      *keyring.TLSKeyring
	serverLeaf   tls.Certificate
	internalLeaf tls.Certificate
}

var (
	_ domain.CAProvider = (*DynamicMintingCA)(nil)
	_ domain.MintingCA  = (*DynamicMintingCA)(nil)
)

// DynamicMintingCAConfig configures a DynamicMintingCA.
type DynamicMintingCAConfig struct {
	Provider *EmbeddedProvider
	Sealer   storage.Sealer
	Logger   *logrus.Logger
}

// NewDynamicMintingCA builds the dynamic CA from the bootstrap minting CA and
// issues the initial server/internal leaves.
func NewDynamicMintingCA(cfg DynamicMintingCAConfig) (*DynamicMintingCA, error) {
	if cfg.Provider == nil {
		return nil, fmt.Errorf("dynamic minting CA: provider is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = logrus.New()
	}
	d := &DynamicMintingCA{provider: cfg.Provider, sealer: cfg.Sealer, logger: cfg.Logger}

	ca, err := cfg.Provider.loadOrCreateCA()
	if err != nil {
		return nil, err
	}
	serverLeaf, err := cfg.Provider.ServerTLSCert()
	if err != nil {
		return nil, err
	}
	internalLeaf, err := cfg.Provider.InternalServerTLSCert()
	if err != nil {
		return nil, err
	}
	pool := ca.CertPool()
	d.state.Store(&dynamicMintingState{active: ca, pool: pool, serverLeaf: serverLeaf, internalLeaf: internalLeaf})
	d.internalProvider = rafttransport.NewDynamicTLSProvider(internalLeaf, pool, pool, false)
	d.forwardProvider = rafttransport.NewDynamicTLSProvider(tls.Certificate{}, pool, pool, false)
	d.embeddedProvider = rafttransport.NewDynamicTLSProvider(serverLeaf, pool, pool, false)
	return d, nil
}

// MintingCA returns the current active minting CA.
func (d *DynamicMintingCA) MintingCA() (domain.MintingCA, error) {
	st := d.state.Load()
	if st == nil || st.active == nil {
		return nil, fmt.Errorf("minting CA not ready")
	}
	return st.active, nil
}

// MintClientCert issues an engine client certificate under the active CA.
func (d *DynamicMintingCA) MintClientCert(commonName string) (*domain.SerializableCertificate, error) {
	st := d.state.Load()
	if st == nil || st.active == nil {
		return nil, fmt.Errorf("minting CA not ready")
	}
	return st.active.MintClientCert(commonName)
}

// CertPool returns the union trust pool of every key in the current keyring.
func (d *DynamicMintingCA) CertPool() *x509.CertPool {
	st := d.state.Load()
	if st == nil || st.pool == nil {
		return x509.NewCertPool()
	}
	return st.pool
}

// ServerTLSCert returns the current embedded data-plane server certificate.
func (d *DynamicMintingCA) ServerTLSCert() (tls.Certificate, error) {
	st := d.state.Load()
	if st == nil {
		return tls.Certificate{}, fmt.Errorf("minting CA not ready")
	}
	return st.serverLeaf, nil
}

// InternalServerTLSCert returns the current internal-listener leaf.
func (d *DynamicMintingCA) InternalServerTLSCert() (tls.Certificate, error) {
	st := d.state.Load()
	if st == nil {
		return tls.Certificate{}, fmt.Errorf("minting CA not ready")
	}
	return st.internalLeaf, nil
}

// InternalTLSProvider returns the dynamic provider for the internal listener.
func (d *DynamicMintingCA) InternalTLSProvider() *rafttransport.DynamicTLSProvider {
	return d.internalProvider
}

// ForwardTLSProvider returns the dynamic provider for the follower->leader
// forward hop (client RootCAs = union pool).
func (d *DynamicMintingCA) ForwardTLSProvider() *rafttransport.DynamicTLSProvider {
	return d.forwardProvider
}

// EmbeddedServerTLSProvider returns the dynamic provider for the embedded
// data-plane server certificate.
func (d *DynamicMintingCA) EmbeddedServerTLSProvider() *rafttransport.DynamicTLSProvider {
	return d.embeddedProvider
}

// ActiveCAPEM returns the settled active Minting CA (key ID, keyring term,
// cert PEM, unsealed key PEM). ok is false when no keyring is installed yet.
func (d *DynamicMintingCA) ActiveCAPEM() (keyID string, term uint64, certPEM, keyPEM []byte, ok bool) {
	st := d.state.Load()
	if st == nil || st.keyring == nil || st.active == nil {
		return "", 0, nil, nil, false
	}
	active := st.keyring.GetActive()
	if active == nil {
		return "", 0, nil, nil, false
	}
	keyPEM, err := d.unseal(active.KeyPEM)
	if err != nil {
		return "", 0, nil, nil, false
	}
	return active.ID, st.keyring.Term, active.CertPEM, keyPEM, true
}

// TrustBundlePEM returns the public Minting CA trust bundle as a concatenated
// PEM of every key in the current keyring (certificates only — never the
// private keys), suitable for a Gateway API BackendTLSPolicy Core ConfigMap
// ca.crt. During phased rotation it yields the full dual-trust bundle
// ([M0, M1]) at the stage/activate terms so a load-balanced control Service
// keeps verifying followers that still present the old leaf. ok is false when
// no CA is available yet.
func (d *DynamicMintingCA) TrustBundlePEM() ([]byte, bool) {
	st := d.state.Load()
	if st == nil {
		return nil, false
	}
	if st.keyring != nil {
		var buf bytes.Buffer
		for _, k := range st.keyring.Keys {
			if k == nil || len(k.CertPEM) == 0 {
				continue
			}
			buf.Write(bytes.TrimSpace(k.CertPEM))
			buf.WriteByte('\n')
		}
		if buf.Len() == 0 {
			return nil, false
		}
		return buf.Bytes(), true
	}
	if st.active != nil {
		certPEM, _, err := st.active.EncodePEM()
		if err != nil || len(certPEM) == 0 {
			return nil, false
		}
		return certPEM, true
	}
	return nil, false
}

// SetKeyringJSON installs a replicated keyring JSON document, updating the
// active CA, the union pool, and the server/internal leaves atomically.
func (d *DynamicMintingCA) SetKeyringJSON(value string) error {
	if value == "" {
		return nil
	}
	var kr keyring.TLSKeyring
	if err := json.Unmarshal([]byte(value), &kr); err != nil {
		return fmt.Errorf("decode minting keyring: %w", err)
	}
	return d.applyKeyring(&kr)
}

func (d *DynamicMintingCA) applyKeyring(kr *keyring.TLSKeyring) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	active := kr.GetActive()
	if active == nil {
		return fmt.Errorf("keyring has no active key")
	}
	keyPEM, err := d.unseal(active.KeyPEM)
	if err != nil {
		return err
	}
	ca, err := NewMintingCAFromPEM(active.CertPEM, keyPEM, d.provider.clientCertTTL)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	for _, k := range kr.Keys {
		if k == nil {
			continue
		}
		pool.AppendCertsFromPEM(k.CertPEM)
	}

	prev := d.state.Load()
	serverLeaf := prev.serverLeaf
	internalLeaf := prev.internalLeaf
	if prev.active == nil || !sameCert(prev.active.CACertificate(), ca.CACertificate()) {
		serverSANs, serverIPs := d.provider.requiredServerSANs()
		serverLeaf, err = d.provider.issueOrReuseServerCert(ca, "server.crt", "server.key", "supervisor-server", serverSANs, serverIPs)
		if err != nil {
			return err
		}
		internalLeaf, err = d.provider.issueOrReuseServerCert(ca, "server-internal.crt", "server-internal.key", "supervisor-internal", d.provider.internalServerSANs(), nil)
		if err != nil {
			return err
		}
	}

	d.state.Store(&dynamicMintingState{
		active:       ca,
		pool:         pool,
		keyring:      kr.Clone(),
		serverLeaf:   serverLeaf,
		internalLeaf: internalLeaf,
	})
	d.embeddedProvider.Update(serverLeaf, pool, pool)
	d.internalProvider.Update(internalLeaf, pool, pool)
	d.forwardProvider.Update(tls.Certificate{}, pool, pool)
	d.logger.WithFields(logrus.Fields{
		"term":          kr.Term,
		"active_key_id": kr.ActiveKeyID,
		"keys":          len(kr.Keys),
	}).Info("minting CA keyring installed")
	return nil
}

func (d *DynamicMintingCA) unseal(stored string) ([]byte, error) {
	if stored == "" {
		return nil, nil
	}
	if d.sealer == nil {
		return []byte(stored), nil
	}
	plain, err := d.sealer.OpenString(stored)
	if err != nil {
		return nil, fmt.Errorf("unseal minting CA key: %w", err)
	}
	return []byte(plain), nil
}

// sameCert reports whether two certificates are the same (by raw DER).
func sameCert(a, b *x509.Certificate) bool {
	if a == nil || b == nil {
		return false
	}
	return bytes.Equal(a.Raw, b.Raw)
}

// MintingRotatorConfig configures a MintingRotator.
type MintingRotatorConfig struct {
	Store          *RaftStore
	CA             *DynamicMintingCA
	Bootstrap      pki.BootstrapProvider
	Sealer         storage.Sealer
	ClientCertTTL  time.Duration
	RotationPeriod time.Duration
	CheckInterval  time.Duration
	CommonName     string
	Organization   string
	DNSNames       []string
	IPAddrs        []net.IP
	CAValidity     time.Duration
	Logger         *logrus.Logger
	Now            func() time.Time
}

// MintingRotator runs the leader-only phased Minting CA rotation loop.
type MintingRotator struct {
	cfg MintingRotatorConfig
}

// NewMintingRotator returns a MintingRotator.
func NewMintingRotator(cfg *MintingRotatorConfig) *MintingRotator {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = 30 * time.Second
	}
	if cfg.RotationPeriod <= 0 {
		cfg.RotationPeriod = 24 * time.Hour
	}
	if cfg.CAValidity <= 0 {
		cfg.CAValidity = 30 * 365 * 24 * time.Hour
	}
	if cfg.Organization == "" {
		cfg.Organization = mintingCAOrganization
	}
	if cfg.Logger == nil {
		cfg.Logger = logrus.New()
	}
	return &MintingRotator{cfg: *cfg}
}

// Start runs the rotation loop until ctx is cancelled.
func (r *MintingRotator) Start(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.CheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if r.cfg.Store == nil || !r.cfg.Store.IsLeader() {
				continue
			}
			if err := r.RotateOnce(ctx); err != nil {
				r.cfg.Logger.WithError(err).Warn("minting CA rotation failed")
			}
		}
	}
}

// RotateOnce advances the keyring by at most one phase.
func (r *MintingRotator) RotateOnce(ctx context.Context) error {
	kr, err := r.readKeyring()
	if err != nil {
		return err
	}
	if kr == nil {
		return r.seed(ctx)
	}
	switch kr.Term {
	case 1:
		return r.stage(ctx, kr)
	case 2:
		return r.activate(ctx, kr)
	case 3:
		return r.prune(ctx, kr)
	default:
		return nil
	}
}

func (r *MintingRotator) readKeyring() (*keyring.TLSKeyring, error) {
	raw, err := r.cfg.Store.fsmRead().readMeta(mintingKeyringMetaKey)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var kr keyring.TLSKeyring
	if err := json.Unmarshal([]byte(raw), &kr); err != nil {
		return nil, fmt.Errorf("decode minting keyring: %w", err)
	}
	return &kr, nil
}

// seed installs the Term=1 keyring from the bootstrap CA.
func (r *MintingRotator) seed(ctx context.Context) error {
	if r.cfg.Bootstrap == nil {
		return nil
	}
	mat, err := r.cfg.Bootstrap.EnsureBootstrapMaterial(ctx)
	if err != nil {
		return err
	}
	kr, err := keyring.KeyringFromBootstrapCA(raftconfig.PKIModeSharedCA, mat, r.cfg.Sealer, r.cfg.Now())
	if err != nil {
		return err
	}
	return r.writeKeyring(ctx, kr)
}

// stage generates M1 and commits the dual-trust keyring (Term=2).
func (r *MintingRotator) stage(ctx context.Context, kr *keyring.TLSKeyring) error {
	active := kr.GetActive()
	if active == nil {
		return fmt.Errorf("keyring has no active key")
	}
	if r.cfg.Now().Sub(active.CreatedTime) < r.cfg.RotationPeriod {
		return nil
	}
	key, err := keyring.GenerateTLSKeyWithGoca(raftconfig.PKIModeSharedCA, r.cfg.CommonName, r.cfg.Organization, r.cfg.DNSNames, r.cfg.IPAddrs, r.cfg.CAValidity, r.cfg.Sealer, r.cfg.Now())
	if err != nil {
		return err
	}
	next := kr.Clone()
	next.Keys = append(next.Keys, key)
	next.Term = 2
	next.AppliedIndex = 0
	if err := r.writeKeyring(ctx, next); err != nil {
		return err
	}
	// Record the applied index so followers can be tracked before activation.
	next.AppliedIndex = r.cfg.Store.AppliedIndex()
	if next.AppliedIndex == 0 {
		next.AppliedIndex = 1
	}
	return r.writeKeyring(ctx, next)
}

// activate promotes M1 as the signer while keeping M0 trusted (Term=3).
func (r *MintingRotator) activate(ctx context.Context, kr *keyring.TLSKeyring) error {
	if len(kr.Keys) != 2 {
		return nil
	}
	if kr.AppliedIndex == 0 {
		// Crash recovery: the stage's second write (recording the applied
		// index) never landed, leaving Term=2 with AppliedIndex=0. Record the
		// applied index now so activation can proceed on the next tick.
		next := kr.Clone()
		next.AppliedIndex = r.cfg.Store.AppliedIndex()
		if next.AppliedIndex == 0 {
			next.AppliedIndex = 1
		}
		return r.writeKeyring(ctx, next)
	}
	if r.cfg.Store.MinFollowerAppliedIndex() < kr.AppliedIndex {
		return nil
	}
	next := kr.Clone()
	next.ActiveKeyID = kr.Keys[1].ID
	next.Term = 3
	next.AppliedIndex = 0
	return r.writeKeyring(ctx, next)
}

// prune drops M0 once every engine client cert minted under it has expired
// (Term=4).
func (r *MintingRotator) prune(ctx context.Context, kr *keyring.TLSKeyring) error {
	if len(kr.Keys) != 2 {
		return nil
	}
	active := kr.GetActive()
	if active == nil {
		return nil
	}
	overlap := 2 * r.cfg.ClientCertTTL
	if overlap < time.Hour {
		overlap = time.Hour
	}
	if r.cfg.Now().Sub(active.CreatedTime) < overlap {
		return nil
	}
	next := &keyring.TLSKeyring{
		Keys:         []*keyring.TLSKey{active},
		ActiveKeyID:  active.ID,
		Term:         4,
		AppliedIndex: 0,
	}
	return r.writeKeyring(ctx, next)
}

func (r *MintingRotator) writeKeyring(ctx context.Context, kr *keyring.TLSKeyring) error {
	data, err := json.Marshal(kr)
	if err != nil {
		return fmt.Errorf("marshal minting keyring: %w", err)
	}
	return r.cfg.Store.applyCtx(ctx, kindSetMeta, cmdSetMeta{Key: mintingKeyringMetaKey, Value: string(data)})
}

// DataplaneFileWatcher wraps a go-raft transport.FileWatcher for the
// cert-manager/external data-plane server certificate. It hot-reloads renewed
// cert/key files and exposes a DynamicTLSProvider for per-handshake use.
type DataplaneFileWatcher struct {
	watcher *rafttransport.FileWatcher
}

// NewDataplaneFileWatcher starts a file watcher over certFile/keyFile/caFile.
func NewDataplaneFileWatcher(certFile, keyFile, caFile string, pollInterval time.Duration, logger *logrus.Logger) (*DataplaneFileWatcher, error) {
	w, err := rafttransport.NewFileWatcher(certFile, keyFile, caFile, pollInterval, pki.DefaultSafetyMargin, false, raftobserv.NewLogrusAdapter(logger))
	if err != nil {
		return nil, err
	}
	return &DataplaneFileWatcher{watcher: w}, nil
}

// Provider returns the dynamic TLS provider fed by the watcher.
func (w *DataplaneFileWatcher) Provider() *rafttransport.DynamicTLSProvider {
	return w.watcher.Provider()
}

// Start polls the files until ctx is cancelled.
func (w *DataplaneFileWatcher) Start(ctx context.Context) {
	w.watcher.Start(ctx)
}
