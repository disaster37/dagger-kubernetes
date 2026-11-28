package domain

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestBuildPackIDDeterministic(t *testing.T) {
	members := []PackMember{
		{Digest: "sha256:" + strings.Repeat("a", 64), Size: 3},
		{Digest: "sha256:" + strings.Repeat("b", 64), Size: 5},
	}
	a, err := BuildPackID(members)
	if err != nil {
		t.Fatalf("BuildPackID: %v", err)
	}
	b, err := BuildPackID(members)
	if err != nil {
		t.Fatalf("BuildPackID: %v", err)
	}
	if a.Digest != b.Digest {
		t.Fatalf("digest not deterministic: %s vs %s", a.Digest, b.Digest)
	}
	if !strings.HasPrefix(a.Digest, "sha256:") {
		t.Fatalf("digest = %q, want sha256:<hex>", a.Digest)
	}
	if len(a.Members) != 2 {
		t.Fatalf("members = %v", a.Members)
	}
	if a.Version != DaggerPackVersion {
		t.Fatalf("version = %q", a.Version)
	}
	members[0].Size = 4
	if c, _ := BuildPackID(members); c.Digest == a.Digest {
		t.Fatal("pack ID must change with a member size")
	}
	if _, err := BuildPackID(nil); err == nil {
		t.Fatal("empty pack must fail")
	}
}

// TestRCGoldenFrames decodes captured beta.28 wire shapes and re-marshals them
// byte-for-byte. The bodies are kept opaque in the envelope, so a round trip
// must preserve them exactly.
func TestRCGoldenFrames(t *testing.T) {
	recipe := "sha256:" + strings.Repeat("1", 64)
	blob := "sha256:" + strings.Repeat("2", 64)
	frames := []string{
		`{"id":1,"type":"hello","body":{"cacheID":"cache-1","generation":1,"engineVersion":"v1.0.0-beta.28","engineName":"engine-1","period":2}}`,
		`{"id":2,"re":1,"type":"welcome","body":{}}`,
		`{"id":2,"type":"open","body":{"period":3}}`,
		`{"id":3,"re":2,"type":"opened","body":{}}`,
		`{"id":3,"type":"settle","body":{"period":3,"sessions":[{"id":"sess-1","marker":true}]}}`,
		`{"id":4,"re":3,"type":"settled","body":{}}`,
		`{"id":4,"type":"claims","body":{"sessions":[{"id":"sess-1","claim":"claim-1"}]}}`,
		`{"id":5,"re":4,"type":"claimed","body":{}}`,
		`{"id":5,"type":"export","body":{"roots":[7,8]}}`,
		`{"id":6,"re":5,"type":"exported","body":{"exportID":"e1","bundle":{"version":1,"roots":[],"values":[]},"skipped":[{"number":8,"reason":"gone"}]}}`,
		`{"id":6,"type":"upload","body":{"exportID":"e1","urls":{"` + blob + `":"https://s3.example/blob"},"merged":true}}`,
		`{"id":7,"re":6,"type":"uploaded","body":{"done":["` + blob + `"],"failed":[{"digest":"` + recipe + `","message":"boom"}]}}`,
		`{"id":7,"type":"lookup","body":{"misses":[{"recipe":"` + recipe + `","extra":["` + blob + `"],"field":"foo","self":"` + recipe + `"}]}}`,
		`{"id":8,"re":7,"type":"lookedUp","body":{"misses":[{"status":"merging","merge":9,"parts":"complete"}]}}`,
		`{"id":9,"type":"merge","body":{"bundle":{"version":1,"roots":[],"values":[]},"lookup":7}}`,
		`{"id":10,"re":9,"type":"merged","body":{"generation":1,"engineTime":123,"values":[],"retained":[],"stale":[]}}`,
		`{"id":10,"type":"renew","body":{"renewalKey":"k","layers":[],"neededBlob":"` + blob + `","packs":{"` + blob + `":"` + recipe + `"}}}`,
		`{"id":11,"re":10,"type":"renewed","body":{"addresses":{"` + blob + `":{"url":"https://s3.example/get","expiresAtUnix":99}}}}`,
		`{"id":11,"type":"offer","body":{"items":[]}}`,
		`{"id":12,"re":11,"type":"offered","body":{"items":[]}}`,
		`{"id":12,"re":11,"type":"error","body":{"message":"stale settle"}}`,
	}
	for _, frame := range frames {
		env, err := DecodeRCEnvelope([]byte(frame))
		if err != nil {
			t.Fatalf("decode %s: %v", frame, err)
		}
		got, err := env.Marshal()
		if err != nil {
			t.Fatalf("marshal %s: %v", frame, err)
		}
		if string(got) != frame {
			t.Errorf("round trip mismatch:\nwant %s\ngot  %s", frame, got)
		}
	}
}

// TestRCCodecRoundTrip encodes, decodes and re-decodes every body type.
func TestRCCodecRoundTrip(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		typ  string
		body any
	}{
		{RCTypeHello, Hello{CacheID: "c", Generation: 2, EngineVersion: "v1.0.0-beta.28", EngineName: "e", Period: 3, Settled: true}},
		{RCTypeWelcome, Welcome{}},
		{RCTypeOpen, Open{Period: 4}},
		{RCTypeOpened, Opened{}},
		{RCTypeSettle, Settle{Period: 4, Sessions: []SettleSession{{ID: "s", Marker: true}}}},
		{RCTypeSettled, Settled{}},
		{RCTypeClaims, Claims{Sessions: []SessionClaim{{ID: "s", Claim: "c"}}}},
		{RCTypeClaimed, Claimed{}},
		{RCTypeExport, Export{Roots: []uint64{1, 2}}},
		{RCTypeExported, Exported{ExportID: "e", Bundle: json.RawMessage(`{"version":1,"roots":[],"values":[]}`), Skipped: []SkippedRoot{{Number: 2, Reason: "gone"}}}},
		{RCTypeUpload, Upload{ExportID: "e", URLs: map[string]string{digest: "https://x"}, Packs: []UploadPack{{Pack: digest, URL: "https://y", Members: []RCPackMember{{Digest: digest, Size: 1}}}}, Merged: true}},
		{RCTypeUploaded, Uploaded{Done: []string{digest}, Failed: []UploadFailure{{Digest: digest, Message: "m"}}}},
		{RCTypeMerge, Merge{Bundle: json.RawMessage(`{"version":1}`), Lookup: 7}},
		{RCTypeMerged, Merged{Generation: 1, EngineTime: 2, Values: []json.RawMessage{}, Retained: []json.RawMessage{}, Stale: []json.RawMessage{}}},
		{RCTypeOffer, Offer{Items: []json.RawMessage{}}},
		{RCTypeOffered, Offered{Items: []json.RawMessage{}}},
		{RCTypeRenew, Renew{RenewalKey: "k", NeededBlob: digest, Packs: map[string]string{digest: digest}}},
		{RCTypeRenewed, Renewed{Addresses: map[string]RCBlobAddress{digest: {URL: "u", ExpiresAtUnix: 5}}}},
		{RCTypeLookup, Lookup{Misses: []LookupMiss{{Recipe: digest, Extra: []string{digest}, Self: digest, Inputs: []string{digest}, Field: "f"}}}},
		{RCTypeLookedUp, LookedUp{Misses: []LookedUpMiss{{Status: RCLookupMerging, Merge: 9, Parts: RCLookupPartsComplete}}}},
		{RCTypeError, RemoteCacheError{Message: "stale settle"}},
	}
	for _, tt := range tests {
		t.Run(tt.typ, func(t *testing.T) {
			env, err := NewRCEnvelope(11, 7, tt.typ, tt.body)
			if err != nil {
				t.Fatalf("NewRCEnvelope: %v", err)
			}
			raw, err := env.Marshal()
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			decoded, err := DecodeRCEnvelope(raw)
			if err != nil {
				t.Fatalf("DecodeRCEnvelope: %v", err)
			}
			if decoded.ID != 11 || decoded.Re != 7 || decoded.Type != tt.typ {
				t.Fatalf("envelope = %+v, want id 11 re 7 type %s", decoded, tt.typ)
			}
			target := reflect.New(reflect.TypeOf(tt.body)).Interface()
			if err := decoded.DecodeBody(target); err != nil {
				t.Fatalf("DecodeBody: %v", err)
			}
			if got := reflect.ValueOf(target).Elem().Interface(); !reflect.DeepEqual(got, tt.body) {
				t.Fatalf("body round trip = %#v, want %#v", got, tt.body)
			}
		})
	}
}
