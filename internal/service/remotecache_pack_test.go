package service

import "testing"

func TestPackBuilderLimits(t *testing.T) {
	b := NewPackBuilder()
	if !b.Add("a", 1) {
		t.Fatal("first add failed")
	}
	if b.Len() != 1 || b.Bytes() != 1 {
		t.Fatalf("len=%d bytes=%d", b.Len(), b.Bytes())
	}
	if PackHeader(b.ID()) == "" {
		t.Fatal("empty header")
	}
}
