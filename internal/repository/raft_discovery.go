package repository

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// defaultRaftPort is the fallback Raft port when bind_addr carries no port.
const defaultRaftPort = 8081

// PeerResolver computes the full voter list for bootstrap/join and identifies
// this node's own peer entry.
type PeerResolver interface {
	// Resolve returns the full ordered voter list (including self).
	Resolve() ([]RaftPeer, error)
	// Self returns this node's peer entry (by ID or advertise address).
	Self() (RaftPeer, error)
}

// RaftDiscoveryConfig is the subset of RaftConfig driving discovery.
type RaftDiscoveryConfig struct {
	NodeID          string
	AdvertiseAddr   string // host:port; "" = derive from hostname+headless+port
	BindAddr        string // host:port (fallback for advertise derivation)
	Peers           []RaftPeer
	Replicas        int
	StatefulSetName string
	HeadlessService string
	Namespace       string
	ClusterDomain   string
	RaftPort        int // from bind_addr; default 8081
}

// NewPeerResolver selects a resolver from config:
//   - if cfg.Peers is non-empty → staticPeerResolver (explicit override).
//   - else if cfg.StatefulSetName != "" && cfg.HeadlessService != "" → dnsPeerResolver.
//   - else → singleNodeResolver (self only, from cfg.AdvertiseAddr/BindAddr).
func NewPeerResolver(cfg *RaftDiscoveryConfig) PeerResolver {
	switch {
	case len(cfg.Peers) > 0:
		return &staticPeerResolver{cfg: *cfg}
	case cfg.StatefulSetName != "" && cfg.HeadlessService != "":
		return &dnsPeerResolver{cfg: *cfg, clusterDomain: clusterDomain(cfg)}
	default:
		return &singleNodeResolver{cfg: *cfg}
	}
}

// clusterDomain returns the configured cluster DNS suffix. The config loader
// defaults raft.cluster_domain to "cluster.local" (standard clusters); an
// explicitly empty value makes peer addresses end at ".svc" with no cluster
// suffix.
func clusterDomain(cfg *RaftDiscoveryConfig) string {
	return cfg.ClusterDomain
}

// raftPort returns the configured Raft port, defaulting to 8081.
func raftPort(cfg *RaftDiscoveryConfig) int {
	if cfg.RaftPort != 0 {
		return cfg.RaftPort
	}
	if _, portStr, err := net.SplitHostPort(cfg.BindAddr); err == nil {
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
			return p
		}
	}
	return defaultRaftPort
}

// podAddress builds a pod's stable DNS name + port for a given hostname:
//   - clusterDomain set:   <host>.<headless>.<ns>.svc.<clusterDomain>:<port>
//   - clusterDomain empty: <host>.<headless>.<ns>.svc:<port>
//
// The FQDN form (clusterDomain set, e.g. "cluster.local") is preferred over
// short names (ending at .svc) because it reduces the NXDOMAIN cache window
// during bootstrap:
//
//   - Short names go through search-domain resolution. The final as-is query
//     (e.g. <pod>.<headless>.<ns>.svc) does not match the cluster.local zone
//     and hits the catch-all .:53 zone in node-local-dns, which uses
//     "cache 30" — stale NXDOMAIN for up to 30 s (negative-cache poisoning).
//
//   - FQDN (e.g. <pod>.<headless>.<ns>.svc.cluster.local) matches the
//     cluster.local zone directly. The node-local-dns Corefile serves this
//     zone with "denial 9984 5" — NXDOMAIN cached for only 5 s. Combined
//     with the cluster CoreDNS kubernetes plugin (authoritative, no cache),
//     the total poison window is at most 5 s vs. 30 s.
//
// Per-node DNS caches (Cilium NodeLocal DNSCache with LRP, or the standard
// node-local-dns addon) amplify the difference: each node independently
// caches NXDOMAIN, so the 30 s window multiplies across nodes.
func podAddress(cfg *RaftDiscoveryConfig, clusterDomain, host string) string {
	name := fmt.Sprintf("%s.%s.%s.svc", host, cfg.HeadlessService, cfg.Namespace)
	if clusterDomain != "" {
		name = fmt.Sprintf("%s.%s", name, clusterDomain)
	}
	return fmt.Sprintf("%s:%d", name, raftPort(cfg))
}

// dnsPeerResolver derives peers from <sts>-<i>.<headless>.<ns>.svc, plus the
// cluster suffix when clusterDomain is non-empty.
type dnsPeerResolver struct {
	cfg           RaftDiscoveryConfig
	clusterDomain string
	// hostname is the pod hostname; "" resolves os.Hostname() at Self() time
	// (set directly by tests to inject a synthetic pod name).
	hostname string
}

func (r *dnsPeerResolver) Resolve() ([]RaftPeer, error) {
	cfg := r.cfg
	if cfg.Replicas < 1 {
		return nil, fmt.Errorf("raft discovery: replicas must be >= 1, got %d", cfg.Replicas)
	}
	if cfg.StatefulSetName == "" {
		return nil, fmt.Errorf("raft discovery: statefulset_name is required")
	}
	if cfg.HeadlessService == "" {
		return nil, fmt.Errorf("raft discovery: headless_service is required")
	}
	if cfg.Namespace == "" {
		return nil, fmt.Errorf("raft discovery: namespace is required")
	}

	peers := make([]RaftPeer, 0, cfg.Replicas)
	for i := 0; i < cfg.Replicas; i++ {
		host := fmt.Sprintf("%s-%d", cfg.StatefulSetName, i)
		peers = append(peers, RaftPeer{
			ID:      host,
			Address: podAddress(&cfg, r.clusterDomain, host),
		})
	}
	return peers, nil
}

func (r *dnsPeerResolver) Self() (RaftPeer, error) {
	host := r.hostname
	if host == "" {
		host, _ = os.Hostname()
	}
	return r.selfFor(host)
}

func (r *dnsPeerResolver) selfFor(host string) (RaftPeer, error) {
	if _, ok := podOrdinal(host, r.cfg.StatefulSetName); ok {
		return RaftPeer{
			ID:      host,
			Address: podAddress(&r.cfg, r.clusterDomain, host),
		}, nil
	}
	if r.cfg.AdvertiseAddr != "" {
		return RaftPeer{ID: r.cfg.NodeID, Address: r.cfg.AdvertiseAddr}, nil
	}
	return RaftPeer{}, fmt.Errorf("raft discovery: hostname %q does not match <statefulset>-<ordinal> and advertise_addr is unset", host)
}

// podOrdinal reports whether host matches "<sts>-<n>" and returns the ordinal.
func podOrdinal(host, sts string) (int, bool) {
	prefix := sts + "-"
	if sts == "" || !strings.HasPrefix(host, prefix) {
		return 0, false
	}
	suffix := strings.TrimPrefix(host, prefix)
	if suffix == "" {
		return 0, false
	}
	n, err := strconv.Atoi(suffix)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// staticPeerResolver returns the explicit raft.peers list.
type staticPeerResolver struct {
	cfg RaftDiscoveryConfig
}

func (r *staticPeerResolver) Resolve() ([]RaftPeer, error) {
	peers := make([]RaftPeer, 0, len(r.cfg.Peers)+1)
	peers = append(peers, r.cfg.Peers...)

	// Ensure self is represented with its authoritative ID/address.
	if _, err := r.matchSelf(); err == nil {
		return peers, nil
	}
	self, err := r.synthesizeSelf()
	if err != nil {
		return nil, err
	}
	return append([]RaftPeer{self}, peers...), nil
}

func (r *staticPeerResolver) Self() (RaftPeer, error) {
	if p, err := r.matchSelf(); err == nil {
		return p, nil
	}
	return r.synthesizeSelf()
}

// matchSelf finds the explicit peer entry matching NodeID, then AdvertiseAddr.
func (r *staticPeerResolver) matchSelf() (RaftPeer, error) {
	cfg := r.cfg
	for _, p := range cfg.Peers {
		if cfg.NodeID != "" && p.ID == cfg.NodeID {
			return p, nil
		}
	}
	if cfg.AdvertiseAddr != "" {
		for _, p := range cfg.Peers {
			if p.Address == cfg.AdvertiseAddr {
				return p, nil
			}
		}
	}
	return RaftPeer{}, fmt.Errorf("raft discovery: self not found in explicit peers")
}

func (r *staticPeerResolver) synthesizeSelf() (RaftPeer, error) {
	cfg := r.cfg
	if cfg.NodeID == "" && cfg.AdvertiseAddr == "" {
		return RaftPeer{}, fmt.Errorf("raft discovery: cannot identify self without node_id or advertise_addr")
	}
	addr := cfg.AdvertiseAddr
	if addr == "" {
		addr = deriveBindHost(cfg.BindAddr, raftPort(&cfg))
	}
	return RaftPeer{ID: cfg.NodeID, Address: addr}, nil
}

// singleNodeResolver represents a one-voter cluster (self only).
type singleNodeResolver struct {
	cfg RaftDiscoveryConfig
}

func (r *singleNodeResolver) Resolve() ([]RaftPeer, error) {
	self, err := r.Self()
	if err != nil {
		return nil, err
	}
	return []RaftPeer{self}, nil
}

func (r *singleNodeResolver) Self() (RaftPeer, error) {
	addr := r.cfg.AdvertiseAddr
	if addr == "" {
		addr = deriveBindHost(r.cfg.BindAddr, raftPort(&r.cfg))
	}
	return RaftPeer{ID: r.cfg.NodeID, Address: addr}, nil
}

// deriveBindHost returns a concrete host:port from bind_addr, defaulting
// 0.0.0.0/::/empty to 127.0.0.1 (single-node advertise fallback).
func deriveBindHost(bindAddr string, port int) string {
	host, portStr, err := net.SplitHostPort(bindAddr)
	if err != nil {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, portStr)
}

// DeriveAdvertiseAddr computes this pod's advertise address:
//   - cfg.AdvertiseAddr if set.
//   - else if hostname matches <sts>-<ordinal>: "<hostname>.<headless>.<ns>.svc[:<clusterDomain>]:<port>".
//   - else: the bind host (warn if 127.0.0.1/0.0.0.0 with multi-node).
func DeriveAdvertiseAddr(cfg *RaftDiscoveryConfig, hostname string) (string, error) {
	return deriveAdvertiseAddr(cfg, hostname)
}

func deriveAdvertiseAddr(cfg *RaftDiscoveryConfig, hostname string) (string, error) {
	if cfg.AdvertiseAddr != "" {
		return cfg.AdvertiseAddr, nil
	}
	if _, ok := podOrdinal(hostname, cfg.StatefulSetName); ok && cfg.HeadlessService != "" && cfg.Namespace != "" {
		return podAddress(cfg, clusterDomain(cfg), hostname), nil
	}
	return deriveBindHost(cfg.BindAddr, raftPort(cfg)), nil
}

// PodSANs returns the DNS + IP SANs for this pod's leaf cert:
//
//	DNS: <hostname>, <hostname>.<headless>, <hostname>.<headless>.<ns>,
//	     <hostname>.<headless>.<ns>.svc,
//	     <hostname>.<headless>.<ns>.svc.<clusterDomain> (when clusterDomain != ""),
//	     "localhost"
//	IP:   127.0.0.1
func PodSANs(cfg *RaftDiscoveryConfig, hostname string) (dnsNames []string, ipAddrs []net.IP) {
	names := []string{hostname, "localhost"}
	if cfg.HeadlessService != "" {
		base := fmt.Sprintf("%s.%s", hostname, cfg.HeadlessService)
		names = append(names, base)
		if cfg.Namespace != "" {
			baseNS := fmt.Sprintf("%s.%s", base, cfg.Namespace)
			names = append(names, baseNS, fmt.Sprintf("%s.svc", baseNS))
			if domain := clusterDomain(cfg); domain != "" {
				names = append(names, fmt.Sprintf("%s.svc.%s", baseNS, domain))
			}
		}
	}
	// Dedupe while preserving order.
	seen := make(map[string]bool, len(names))
	deduped := make([]string, 0, len(names))
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			deduped = append(deduped, n)
		}
	}
	return deduped, []net.IP{net.ParseIP("127.0.0.1")}
}
