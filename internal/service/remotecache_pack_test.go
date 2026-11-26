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
	if decoded.Total != manifest.Total {
		t.Fatalf("decoded total = %d, encoded total = %d", decoded.Total, manifest.Total)
	}
	if decoded.Total != int64(len(pack)) {
		t.Fatalf("decoded total = %d, pack = %d", decoded.Total, len(pack))
	}
	if len(decoded.Members) != len(manifest.Members) {
		t.Fatalf("decoded members = %d, encoded members = %d", len(decoded.Members), len(manifest.Members))
	}
	for i, m := range decoded.Members {
		if m.Offset != manifest.Members[i].Offset || m.Size != manifest.Members[i].Size {
			t.Fatalf("member %d = %+v, want %+v", i, m, manifest.Members[i])
		}
		got := pack[m.Offset : m.Offset+m.Size]
		if !bytes.Equal(got, members[i].Data) {
			t.Fatalf("member %d = %q, want %q", i, got, members[i].Data)
		}
	}
}

func TestPackHeaderLenMatchesEncodePack(t *testing.T) {
	members := []PackMemberPayload{
		{Digest: "sha256:aaa", Data: []byte("hello")},
		{Digest: "sha256:bbb", Data: []byte("world!")},
	}
	_, manifest, err := EncodePack(members)
	if err != nil {
		t.Fatal(err)
	}
	specs := make([]PackMemberSpec, 0, len(members))
	for _, m := range members {
		specs = append(specs, PackMemberSpec{Digest: m.Digest, Size: int64(len(m.Data))})
	}
	headerLen := PackHeaderLen(manifest.ID, specs)
	if headerLen != manifest.Members[0].Offset {
		t.Fatalf("headerLen = %d, first member offset = %d", headerLen, manifest.Members[0].Offset)
	}
	var rel int64
	for i, m := range members {
		want := headerLen + rel
		if manifest.Members[i].Offset != want {
			t.Fatalf("member %d offset = %d, want %d", i, manifest.Members[i].Offset, want)
		}
		rel += int64(len(m.Data))
	}
}
