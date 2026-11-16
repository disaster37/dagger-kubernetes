package service

import (
	"bytes"
	"testing"
)

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

func TestEncodeDecodePack(t *testing.T) {
	members := []PackMemberPayload{
		{Digest: "sha256:aaa", Data: []byte("hello")},
		{Digest: "sha256:bbb", Data: []byte("world!")},
	}
	pack, manifest, err := EncodePack(members)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Total != int64(len(pack)) {
		t.Fatalf("total = %d, pack = %d", manifest.Total, len(pack))
	}
	decoded, headerLen, err := DecodePackHeader(bytes.NewReader(pack))
	if err != nil {
		t.Fatal(err)
	}
	if headerLen <= 0 || decoded.ID.Digest != manifest.ID.Digest {
		t.Fatalf("decoded = %+v headerLen=%d", decoded, headerLen)
	}
	for i, m := range decoded.Members {
		got := pack[m.Offset : m.Offset+m.Size]
		if !bytes.Equal(got, members[i].Data) {
			t.Fatalf("member %d = %q, want %q", i, got, members[i].Data)
		}
	}
}
