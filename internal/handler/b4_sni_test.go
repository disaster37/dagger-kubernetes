package handler

import (
	"crypto/tls"
	"testing"
)

// TestMatchesInternalSNIExactForms verifies that only the EXACT configured
// internal host forms are treated as internal SNIs: an arbitrary ".svc" name
// that is not the configured control/data host must fall back to the public
// certificate, as must the public hostname.
func TestMatchesInternalSNIExactForms(t *testing.T) {
	s := internalServer("https://rel-control.ns.svc:80", "rel-data.ns.svc:443", 0)
	s.cfg.ClusterDomain = "cluster.local"
	cases := []struct {
		name string
		sni  string
		want bool
	}{
		{"control short", "rel-control", true},
		{"control namespaced", "rel-control.ns.svc", true},
		{"control fqdn", "rel-control.ns.svc.cluster.local", true},
		{"data short", "rel-data", true},
		{"data namespaced", "rel-data.ns.svc", true},
		{"data fqdn", "rel-data.ns.svc.cluster.local", true},
		{"arbitrary svc is public", "evil.svc", false},
		{"arbitrary namespaced svc is public", "other.ns.svc", false},
		{"arbitrary fqdn svc is public", "attacker.ns.svc.cluster.local", false},
		{"public hostname", "data.example.com", false},
		{"trailing dot trimmed", "rel-control.ns.svc.", true},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.matchesInternalSNI(tc.sni); got != tc.want {
				t.Errorf("matchesInternalSNI(%q) = %v, want %v", tc.sni, got, tc.want)
			}
		})
	}
}

// TestMatchesInternalSNINoConfiguredHost verifies that with no internal hosts
// configured, every SNI (including ".svc" names) is treated as public.
func TestMatchesInternalSNINoConfiguredHost(t *testing.T) {
	s := internalServer("", "", 0)
	for _, sni := range []string{"svc.ns.svc", "evil.svc", "data.example.com"} {
		if s.matchesInternalSNI(sni) {
			t.Errorf("matchesInternalSNI(%q) = true with no internal host configured", sni)
		}
	}
}

// TestInternalCertPrecedence verifies the cert-selection branch the SNI routing
// feeds: internalCert returns the configured static leaf, or an error when none
// is configured (so the caller falls through to the public certificate).
func TestInternalCertPrecedence(t *testing.T) {
	want := &tls.Certificate{}
	s := internalServer("https://rel-control.ns.svc:80", "rel-data.ns.svc:443", 0)
	s.cfg.InternalTLSCert = want

	got, err := s.internalCert()
	if err != nil {
		t.Fatalf("internalCert: %v", err)
	}
	if got != want {
		t.Errorf("internalCert = %p, want the configured static leaf %p", got, want)
	}

	// Unconfigured: the caller must be able to detect "no internal cert" and
	// fall back to the public cert (the routing decision is matchesInternalSNI).
	s.cfg.InternalTLSCert = nil
	if _, err := s.internalCert(); err == nil {
		t.Error("internalCert with no provider/cert = nil error, want error")
	}
}
