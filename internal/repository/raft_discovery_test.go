package repository

import (
	"fmt"
	"net"
	"testing"
)

func TestNewPeerResolverSelection(t *testing.T) {
	tests := []struct {
		name string
		cfg  RaftDiscoveryConfig
		want string // type name
	}{
		{
			name: "explicit peers wins",
			cfg:  RaftDiscoveryConfig{Peers: []RaftPeer{{ID: "a", Address: "a:1"}}, StatefulSetName: "sts", HeadlessService: "h"},
			want: "*repository.staticPeerResolver",
		},
		{
			name: "dns discovery",
			cfg:  RaftDiscoveryConfig{StatefulSetName: "sts", HeadlessService: "h", Namespace: "ns", Replicas: 3},
			want: "*repository.dnsPeerResolver",
		},
		{
			name: "single node fallback",
			cfg:  RaftDiscoveryConfig{BindAddr: ":8081"},
			want: "*repository.singleNodeResolver",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolver := NewPeerResolver(&tc.cfg)
			got := typeName(resolver)
			if got != tc.want {
				t.Fatalf("resolver type = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestDNSPeerResolverResolve(t *testing.T) {
	tests := []struct {
		name    string
		cfg     RaftDiscoveryConfig
		want    []RaftPeer
		wantErr bool
	}{
		{
			name: "three replicas",
			cfg: RaftDiscoveryConfig{
				StatefulSetName: "dagger-kubernetes-supervisor",
				HeadlessService: "dagger-kubernetes-headless",
				Namespace:       "dagger-kubernetes",
				ClusterDomain:   "cluster.local",
				Replicas:        3,
				RaftPort:        8081,
			},
			want: []RaftPeer{
				{ID: "dagger-kubernetes-supervisor-0", Address: "dagger-kubernetes-supervisor-0.dagger-kubernetes-headless.dagger-kubernetes.svc.cluster.local:8081"},
				{ID: "dagger-kubernetes-supervisor-1", Address: "dagger-kubernetes-supervisor-1.dagger-kubernetes-headless.dagger-kubernetes.svc.cluster.local:8081"},
				{ID: "dagger-kubernetes-supervisor-2", Address: "dagger-kubernetes-supervisor-2.dagger-kubernetes-headless.dagger-kubernetes.svc.cluster.local:8081"},
			},
		},
		{
			name: "single replica",
			cfg: RaftDiscoveryConfig{
				StatefulSetName: "sts",
				HeadlessService: "headless",
				Namespace:       "ns",
				Replicas:        1,
				RaftPort:        8081,
			},
			want: []RaftPeer{{ID: "sts-0", Address: "sts-0.headless.ns.svc.cluster.local:8081"}},
		},
		{
			name:    "zero replicas",
			cfg:     RaftDiscoveryConfig{StatefulSetName: "sts", HeadlessService: "h", Namespace: "ns", Replicas: 0},
			wantErr: true,
		},
		{
			name:    "missing statefulset name",
			cfg:     RaftDiscoveryConfig{HeadlessService: "h", Namespace: "ns", Replicas: 3},
			wantErr: true,
		},
		{
			name:    "missing headless service",
			cfg:     RaftDiscoveryConfig{StatefulSetName: "sts", Namespace: "ns", Replicas: 3},
			wantErr: true,
		},
		{
			name:    "missing namespace",
			cfg:     RaftDiscoveryConfig{StatefulSetName: "sts", HeadlessService: "h", Replicas: 3},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &dnsPeerResolver{cfg: tc.cfg, clusterDomain: clusterDomain(&tc.cfg)}
			got, err := r.Resolve()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Resolve = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("Resolve[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestDNSPeerResolverSelf(t *testing.T) {
	cfg := RaftDiscoveryConfig{
		StatefulSetName: "sts",
		HeadlessService: "headless",
		Namespace:       "ns",
		ClusterDomain:   "cluster.local",
		Replicas:        3,
		RaftPort:        8081,
	}
	r := &dnsPeerResolver{cfg: cfg, clusterDomain: clusterDomain(&cfg)}

	t.Run("matches pod ordinal", func(t *testing.T) {
		self, err := r.selfFor("sts-1")
		if err != nil {
			t.Fatalf("selfFor: %v", err)
		}
		want := RaftPeer{ID: "sts-1", Address: "sts-1.headless.ns.svc.cluster.local:8081"}
		if self != want {
			t.Fatalf("self = %+v, want %+v", self, want)
		}
	})

	t.Run("unknown hostname with advertise", func(t *testing.T) {
		r2 := &dnsPeerResolver{cfg: RaftDiscoveryConfig{
			StatefulSetName: "sts",
			HeadlessService: "headless",
			Namespace:       "ns",
			NodeID:          "custom",
			AdvertiseAddr:   "custom.example:8081",
		}, clusterDomain: "cluster.local"}
		self, err := r2.selfFor("other-host")
		if err != nil {
			t.Fatalf("selfFor: %v", err)
		}
		if self.ID != "custom" || self.Address != "custom.example:8081" {
			t.Fatalf("self = %+v", self)
		}
	})

	t.Run("unknown hostname no advertise errors", func(t *testing.T) {
		if _, err := r.selfFor("other-host"); err == nil {
			t.Fatal("expected error for unknown hostname without advertise")
		}
	})

	t.Run("Self resolves via injected hostname", func(t *testing.T) {
		r3 := &dnsPeerResolver{cfg: cfg, clusterDomain: clusterDomain(&cfg), hostname: "sts-1"}
		self, err := r3.Self()
		if err != nil {
			t.Fatalf("Self: %v", err)
		}
		want := RaftPeer{ID: "sts-1", Address: "sts-1.headless.ns.svc.cluster.local:8081"}
		if self != want {
			t.Fatalf("Self = %+v, want %+v", self, want)
		}
	})

	t.Run("Self falls back to os.Hostname", func(t *testing.T) {
		r4 := &dnsPeerResolver{cfg: cfg, clusterDomain: clusterDomain(&cfg)}
		self, err := r4.Self()
		if err == nil {
			// The test host name usually does not match "<sts>-<ordinal>",
			// but if it happens to, Self must still return a valid peer.
			if self.ID == "" || self.Address == "" {
				t.Fatalf("Self = %+v", self)
			}
		}
	})
}

func TestStaticPeerResolver(t *testing.T) {
	peers := []RaftPeer{
		{ID: "node-1", Address: "node-1.example:8081"},
		{ID: "node-2", Address: "node-2.example:8081"},
	}
	r := &staticPeerResolver{cfg: RaftDiscoveryConfig{Peers: peers, NodeID: "node-2"}}

	got, err := r.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Resolve = %v, want 2 entries", got)
	}
	self, err := r.Self()
	if err != nil {
		t.Fatalf("Self: %v", err)
	}
	if self != peers[1] {
		t.Fatalf("Self = %+v, want %+v (matched by NodeID)", self, peers[1])
	}

	// Match by AdvertiseAddr when NodeID is unset.
	r2 := &staticPeerResolver{cfg: RaftDiscoveryConfig{Peers: peers, AdvertiseAddr: "node-1.example:8081"}}
	self2, err := r2.Self()
	if err != nil {
		t.Fatalf("Self (by addr): %v", err)
	}
	if self2 != peers[0] {
		t.Fatalf("Self (by addr) = %+v, want %+v", self2, peers[0])
	}

	// Self not listed → synthesized from NodeID/AdvertiseAddr.
	r3 := &staticPeerResolver{cfg: RaftDiscoveryConfig{Peers: peers, NodeID: "node-3", AdvertiseAddr: "node-3.example:8081"}}
	resolved, err := r3.Resolve()
	if err != nil {
		t.Fatalf("Resolve (synthesized): %v", err)
	}
	if len(resolved) != 3 || resolved[0].ID != "node-3" {
		t.Fatalf("Resolve (synthesized) = %v, want self prepended", resolved)
	}

	// No NodeID and no AdvertiseAddr → error.
	r4 := &staticPeerResolver{cfg: RaftDiscoveryConfig{Peers: peers}}
	if _, err := r4.Self(); err == nil {
		t.Fatal("expected error when self cannot be identified")
	}
}

func TestSingleNodeResolver(t *testing.T) {
	r := &singleNodeResolver{cfg: RaftDiscoveryConfig{BindAddr: ":8081", NodeID: "solo"}}
	peers, err := r.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("Resolve = %v, want 1", peers)
	}
	if peers[0].ID != "solo" || peers[0].Address != "127.0.0.1:8081" {
		t.Fatalf("peer = %+v, want solo@127.0.0.1:8081", peers[0])
	}
}

func TestDeriveAdvertiseAddr(t *testing.T) {
	tests := []struct {
		name     string
		cfg      RaftDiscoveryConfig
		hostname string
		want     string
	}{
		{
			name:     "explicit advertise",
			cfg:      RaftDiscoveryConfig{AdvertiseAddr: "explicit.example:9000"},
			hostname: "sts-2",
			want:     "explicit.example:9000",
		},
		{
			name: "pod fqdn derivation",
			cfg: RaftDiscoveryConfig{
				StatefulSetName: "sts",
				HeadlessService: "headless",
				Namespace:       "ns",
				ClusterDomain:   "cluster.local",
				RaftPort:        8081,
			},
			hostname: "sts-2",
			want:     "sts-2.headless.ns.svc.cluster.local:8081",
		},
		{
			name: "hostname not matching falls back to bind",
			cfg: RaftDiscoveryConfig{
				StatefulSetName: "sts",
				HeadlessService: "headless",
				Namespace:       "ns",
				BindAddr:        "10.0.0.5:8081",
			},
			hostname: "other-host",
			want:     "10.0.0.5:8081",
		},
		{
			name:     "wildcard bind falls back to loopback",
			cfg:      RaftDiscoveryConfig{BindAddr: ":8081"},
			hostname: "other-host",
			want:     "127.0.0.1:8081",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DeriveAdvertiseAddr(&tc.cfg, tc.hostname)
			if err != nil {
				t.Fatalf("DeriveAdvertiseAddr: %v", err)
			}
			if got != tc.want {
				t.Fatalf("advertise = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPodOrdinal(t *testing.T) {
	tests := []struct {
		host string
		sts  string
		n    int
		ok   bool
	}{
		{"sts-0", "sts", 0, true},
		{"sts-12", "sts", 12, true},
		{"sts-x", "sts", 0, false},
		{"sts-", "sts", 0, false},
		{"other-1", "sts", 0, false},
		{"sts-1", "", 0, false},
		{"sts--1", "sts", 0, false},
	}
	for _, tc := range tests {
		n, ok := podOrdinal(tc.host, tc.sts)
		if n != tc.n || ok != tc.ok {
			t.Fatalf("podOrdinal(%q,%q) = (%d,%v), want (%d,%v)", tc.host, tc.sts, n, ok, tc.n, tc.ok)
		}
	}
}

func TestPodSANs(t *testing.T) {
	cfg := RaftDiscoveryConfig{
		HeadlessService: "headless",
		Namespace:       "ns",
		ClusterDomain:   "cluster.local",
	}
	dns, ips := PodSANs(&cfg, "sts-0")
	want := []string{
		"sts-0",
		"localhost",
		"sts-0.headless",
		"sts-0.headless.ns",
		"sts-0.headless.ns.svc",
		"sts-0.headless.ns.svc.cluster.local",
	}
	if len(dns) != len(want) {
		t.Fatalf("podSANs dns = %v, want %v", dns, want)
	}
	for i := range want {
		if dns[i] != want[i] {
			t.Fatalf("podSANs dns[%d] = %q, want %q", i, dns[i], want[i])
		}
	}
	if len(ips) != 1 || !ips[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("podSANs ips = %v, want [127.0.0.1]", ips)
	}

	// No headless service → only hostname + localhost + IP.
	dns2, _ := PodSANs(&RaftDiscoveryConfig{}, "plain-host")
	if len(dns2) != 2 || dns2[0] != "plain-host" || dns2[1] != "localhost" {
		t.Fatalf("podSANs (no headless) = %v", dns2)
	}
}

// typeName returns the dynamic type name of v for resolver-selection tests.
func typeName(v interface{}) string {
	return fmt.Sprintf("%T", v)
}
