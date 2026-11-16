package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Remote cache protocol message types (engine/remotecache/protocol).
const (
	RCTypeHello    = "hello"
	RCTypeWelcome  = "welcome"
	RCTypeOpen     = "open"
	RCTypeOpened   = "opened"
	RCTypeClaims   = "claims"
	RCTypeClaimed  = "claimed"
	RCTypeSettle   = "settle"
	RCTypeSettled  = "settled"
	RCTypeExport   = "export"
	RCTypeExported = "exported"
	RCTypeUpload   = "upload"
	RCTypeUploaded = "uploaded"
	RCTypeMerge    = "merge"
	RCTypeMerged   = "merged"
	RCTypeOffer    = "offer"
	RCTypeOffered  = "offered"
	RCTypeRenew    = "renew"
	RCTypeRenewed  = "renewed"
	RCTypeLookup   = "lookup"
	RCTypeLookedUp = "lookedUp"
	RCTypeError    = "error"
)

// RemoteCacheEnvelope is the JSON wire envelope for every /v1/engine message.
type RemoteCacheEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// NewRemoteCacheEnvelope marshals payload into an envelope.
func NewRemoteCacheEnvelope(msgType string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal remote cache payload: %w", err)
	}
	return json.Marshal(RemoteCacheEnvelope{Type: msgType, Data: data})
}

// DecodeRemoteCacheEnvelope decodes an envelope and its payload into out.
func DecodeRemoteCacheEnvelope(raw []byte, out any) (string, error) {
	var env RemoteCacheEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("decode remote cache envelope: %w", err)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return env.Type, fmt.Errorf("decode remote cache payload: %w", err)
		}
	}
	return env.Type, nil
}

// Hello is the engine's opening handshake.
type Hello struct {
	Version string `json:"version"`
	OrgID   string `json:"org_id,omitempty"`
	Token   string `json:"token,omitempty"`
}

// Welcome acknowledges a Hello.
type Welcome struct {
	Namespace string `json:"namespace"`
	MaxBytes  int64  `json:"max_bytes,omitempty"`
}

// Open opens a cache session for a set of roots.
type Open struct {
	SessionID string   `json:"session_id"`
	Roots     []string `json:"roots,omitempty"`
}

// Opened acknowledges an Open.
type Opened struct {
	SessionID string `json:"session_id"`
}

// Claims declares the digests an engine can serve.
type Claims struct {
	SessionID string   `json:"session_id"`
	Digests   []string `json:"digests"`
}

// Claimed acknowledges Claims.
type Claimed struct {
	SessionID string `json:"session_id"`
}

// Settle reports an executed root that should be exported.
type Settle struct {
	SessionID string `json:"session_id"`
	Root      string `json:"root"`
	Digest    string `json:"digest"`
}

// Settled acknowledges Settle.
type Settled struct {
	Root string `json:"root"`
}

// Export carries a dagql.ValueBundle to store.
type Export struct {
	SessionID string `json:"session_id"`
	Root      string `json:"root"`
	Digest    string `json:"digest"`
	Bundle    []byte `json:"bundle"`
}

// Exported acknowledges Export.
type Exported struct {
	Root string `json:"root"`
	Key  string `json:"key"`
}

// Upload requests presigned PUT URLs for blobs.
type Upload struct {
	SessionID string           `json:"session_id"`
	Digests   []string         `json:"digests"`
	Blobs     []BlobDescriptor `json:"blobs,omitempty"`
	Packs     []PackManifest   `json:"packs,omitempty"`
}

// Uploaded returns presigned PUT URLs.
type Uploaded struct {
	URLs  map[string]string `json:"urls"`
	Packs []UploadPack      `json:"packs,omitempty"`
}

// Merge requests presigned GET URLs for cached blobs.
type Merge struct {
	SessionID string   `json:"session_id"`
	Digests   []string `json:"digests"`
}

// Merged returns presigned GET URLs.
type Merged struct {
	URLs       map[string]string       `json:"urls"`
	Locations  map[string]BlobLocation `json:"locations,omitempty"`
	BundleURLs map[string]string       `json:"bundle_urls,omitempty"`
}

// Offer offers a bundle to the engine.
type Offer struct {
	Root   string `json:"root"`
	Digest string `json:"digest"`
	URL    string `json:"url"`
}

// Offered acknowledges Offer.
type Offered struct {
	Root string `json:"root"`
}

// Renew asks for fresh presigned URLs.
type Renew struct {
	SessionID string   `json:"session_id"`
	Digests   []string `json:"digests"`
}

// Renewed returns fresh presigned GET URLs.
type Renewed struct {
	URLs       map[string]string       `json:"urls"`
	Locations  map[string]BlobLocation `json:"locations,omitempty"`
	BundleURLs map[string]string       `json:"bundle_urls,omitempty"`
}

// Lookup asks whether digests are cached.
type Lookup struct {
	SessionID string   `json:"session_id"`
	Digests   []string `json:"digests"`
}

// LookedUp reports per-digest cache status.
type LookedUp struct {
	Status map[string]string `json:"status"` // merging | notStored | unknown
}

// RemoteCacheError is a protocol error payload.
type RemoteCacheError struct {
	Message string `json:"message"`
}

// ValueBundle is the dagql.ValueBundle JSON shape stored in S3.
type ValueBundle struct {
	Root             string   `json:"root"`
	Digest           string   `json:"digest"`
	ShareChainLayers bool     `json:"share_chain_layers"`
	Layers           []string `json:"layers,omitempty"`
	RecipeDigest     string   `json:"recipe_digest,omitempty"`
	ExtraDigests     []string `json:"extra_digests,omitempty"`
	SelfDigest       string   `json:"self_digest,omitempty"`
	InputDigests     []string `json:"input_digests,omitempty"`
}

// DaggerPackVersion is the dagger-pack wire version.
const DaggerPackVersion = "dagger-pack v1"

// DefaultPackMaxMemberBytes is the threshold for packing small blobs.
const DefaultPackMaxMemberBytes = 4 << 20 // 4 MiB

// PackMember is one member slice inside a dagger-pack object.
type PackMember struct {
	Digest string `json:"digest"`
	Offset int64  `json:"offset"` // byte offset from start of pack object
	Size   int64  `json:"size"`
}

// PackManifest describes the members of one dagger-pack object.
type PackManifest struct {
	ID      PackID       `json:"id"`
	Members []PackMember `json:"members"`
	Total   int64        `json:"total"`
}

// BlobDescriptor is one blob requested for upload.
type BlobDescriptor struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// UploadPack is a presigned pack upload target.
type UploadPack struct {
	PackID  PackID       `json:"pack_id"`
	URL     string       `json:"url"`
	Members []PackMember `json:"members"`
}

// BlobLocation is a presigned GET location for one blob (packed or standalone).
type BlobLocation struct {
	URL        string            `json:"url"`
	PackDigest string            `json:"pack_digest,omitempty"`
	Offset     int64             `json:"offset,omitempty"`
	Size       int64             `json:"size,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"` // {"Range": "bytes=start-end"} for 206 Partial Content
}

// Pack limits.
const (
	MaxPackMembers = 4096
	MaxPackBytes   = 64 << 20
)

// PackID is a deterministic dagger-pack v1 identifier.
type PackID struct {
	Version string   `json:"version"`
	Members []string `json:"members"`
	Digest  string   `json:"digest"`
}

// BuildPackID builds a deterministic pack ID from member digests. Members are
// sorted and deduplicated; the digest is SHA-256 over the canonical member
// list.
func BuildPackID(members []string) PackID {
	seen := make(map[string]bool, len(members))
	unique := make([]string, 0, len(members))
	for _, m := range members {
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		unique = append(unique, m)
	}
	sort.Strings(unique)
	if len(unique) > MaxPackMembers {
		unique = unique[:MaxPackMembers]
	}
	sum := sha256.Sum256([]byte(strings.Join(unique, "\n")))
	return PackID{Version: DaggerPackVersion, Members: unique, Digest: hex.EncodeToString(sum[:])}
}
