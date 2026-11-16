package domain

import "testing"

func TestBuildPackIDDeterministic(t *testing.T) {
	a := BuildPackID([]string{"b", "a", "a"})
	b := BuildPackID([]string{"a", "b"})
	if a.Digest != b.Digest {
		t.Fatalf("digest not deterministic: %s vs %s", a.Digest, b.Digest)
	}
	if len(a.Members) != 2 {
		t.Fatalf("members = %v", a.Members)
	}
	if a.Version != DaggerPackVersion {
		t.Fatalf("version = %q", a.Version)
	}
}

func TestRemoteCacheEnvelopeRoundTrip(t *testing.T) {
	raw, err := NewRemoteCacheEnvelope(RCTypeHello, Hello{Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	var hello Hello
	typ, err := DecodeRemoteCacheEnvelope(raw, &hello)
	if err != nil {
		t.Fatal(err)
	}
	if typ != RCTypeHello || hello.Version != "v1" {
		t.Fatalf("typ=%q hello=%+v", typ, hello)
	}
}
