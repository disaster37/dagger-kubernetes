package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Remote cache protocol constants, matching the Dagger engine's
// engine/remotecache/protocol package at v1.0.0-beta.28.
const (
	// RCMaxMessageBytes is the largest message either side accepts.
	RCMaxMessageBytes = 64 << 20
	// RCPingInterval is how often the service pings the engine.
	RCPingInterval = 15 * time.Second
	// RCPingTimeout is the engine's pong deadline.
	RCPingTimeout = 15 * time.Second
	// RCCloseTokenRevoked is the close status for a revoked token.
	RCCloseTokenRevoked = 4001
	// RCStaleSettle ends a settle for a period that is no longer current.
	RCStaleSettle = "stale settle"
	// RCMaxLookupMisses is the most misses one lookup names.
	RCMaxLookupMisses = 32
	// RCMaxLookupExtras is the most extra digests one miss names.
	RCMaxLookupExtras = 8
)

// Remote cache message types (engine/remotecache/protocol).
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

// Lookup status vocabulary (beta.28 has no "stored" status: a hit is delivered
// as a subsequent merge).
const (
	RCLookupUnknown   = "unknown"
	RCLookupNotStored = "notStored"
	RCLookupInFlight  = "inFlight"
	RCLookupMerging   = "merging"
	RCLookupDeferred  = "deferred"
)

// Lookup parts vocabulary.
const (
	RCLookupPartsComplete = "complete"
	RCLookupPartsPartial  = "partial"
	RCLookupPartsNone     = "none"
)

// RCEnvelope frames every message. ID numbers a message per sender from 1; a
// reply names its request's ID in Re, and Re 0 means the message is not a
// reply.
type RCEnvelope struct {
	ID   uint64          `json:"id"`
	Re   uint64          `json:"re,omitempty"`
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
}

// NewRCEnvelope encodes body into an envelope.
func NewRCEnvelope(id, re uint64, typ string, body any) (RCEnvelope, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return RCEnvelope{}, fmt.Errorf("encode %s body: %w", typ, err)
	}
	return RCEnvelope{ID: id, Re: re, Type: typ, Body: raw}, nil
}

// Marshal encodes the envelope.
func (e RCEnvelope) Marshal() ([]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("marshal %s envelope: %w", e.Type, err)
	}
	return data, nil
}

// DecodeRCEnvelope decodes one wire message.
func DecodeRCEnvelope(raw []byte) (RCEnvelope, error) {
	var env RCEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return RCEnvelope{}, fmt.Errorf("decode remote cache envelope: %w", err)
	}
	return env, nil
}

// DecodeBody decodes the envelope body into v.
func (e RCEnvelope) DecodeBody(v any) error {
	if err := json.Unmarshal(e.Body, v); err != nil {
		return fmt.Errorf("decode %s body: %w", e.Type, err)
	}
	return nil
}

// Hello is the engine's first message on a connection.
type Hello struct {
	CacheID       string `json:"cacheID"`
	Generation    uint64 `json:"generation"`
	EngineVersion string `json:"engineVersion"`
	EngineName    string `json:"engineName"`
	Period        uint64 `json:"period"`
	Settled       bool   `json:"settled,omitempty"`
}

// Welcome is the service's reply to Hello.
type Welcome struct{}

// Open tells the service the engine's idle period began.
type Open struct {
	Period uint64 `json:"period"`
}

// Opened is the service's reply to Open.
type Opened struct{}

// Settle asks the service to finish what it wants of the engine for Period.
type Settle struct {
	Period   uint64          `json:"period"`
	Sessions []SettleSession `json:"sessions"`
}

// SettleSession is one ended engine session.
type SettleSession struct {
	ID     string `json:"id"`
	Marker bool   `json:"marker"`
}

// Settled is the service's reply to Settle.
type Settled struct{}

// Claims tells the service which Dagger Cloud claim each session started under.
type Claims struct {
	Sessions []SessionClaim `json:"sessions"`
}

// SessionClaim is one session and its claim.
type SessionClaim struct {
	ID    string `json:"id"`
	Claim string `json:"claim"`
}

// Claimed is the service's reply to Claims.
type Claimed struct{}

// Export asks the engine for the values of some of its entries, by number. The
// service never selects outputs, so Outputs is left empty.
type Export struct {
	Roots   []uint64       `json:"roots"`
	Outputs []ExportOutput `json:"outputs,omitempty"`
}

// ExportOutput selects one part of an entry for upload.
type ExportOutput struct {
	Number  uint64          `json:"number"`
	Address json.RawMessage `json:"address"`
}

// Exported answers Export. Bundle is the dagql ValueBundle JSON, kept opaque:
// the service stores and later replays it verbatim.
type Exported struct {
	ExportID string          `json:"exportID,omitempty"`
	Bundle   json.RawMessage `json:"bundle,omitempty"`
	Skipped  []SkippedRoot   `json:"skipped,omitempty"`
}

// SkippedRoot is an exported root left out, by number.
type SkippedRoot struct {
	Number  uint64 `json:"number"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

// Upload gives the engine one presigned PUT per blob the service needs from an
// export.
type Upload struct {
	ExportID string            `json:"exportID"`
	URLs     map[string]string `json:"urls"`
	Packs    []UploadPack      `json:"packs,omitempty"`
	Merged   bool              `json:"merged,omitempty"`
}

// UploadPack is one pack to upload: its ID, its presigned PUT, and its members
// in order.
type UploadPack struct {
	Pack    string         `json:"pack"`
	URL     string         `json:"url"`
	Members []RCPackMember `json:"members"`
}

// RCPackMember is one blob of a pack on the wire.
type RCPackMember struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// Uploaded answers Upload.
type Uploaded struct {
	Done   []string        `json:"done"`
	Failed []UploadFailure `json:"failed,omitempty"`
}

// UploadFailure is one blob or pack the engine could not upload.
type UploadFailure struct {
	Digest  string `json:"digest"`
	Message string `json:"message"`
}

// Merge gives the engine a bundle of the service's values.
type Merge struct {
	Bundle json.RawMessage `json:"bundle"`
	Lookup uint64          `json:"lookup"`
}

// Merged answers Merge.
type Merged struct {
	Generation uint64            `json:"generation"`
	EngineTime int64             `json:"engineTime"`
	Values     []json.RawMessage `json:"values"`
	Retained   []json.RawMessage `json:"retained"`
	Skipped    []json.RawMessage `json:"skipped,omitempty"`
	Stale      []json.RawMessage `json:"stale"`
}

// Offer places the service's parts on the engine's own entries. The platform
// does not place parts, so only the reply path is decoded.
type Offer struct {
	Items []json.RawMessage `json:"items"`
}

// Offered answers Offer.
type Offered struct {
	Items []json.RawMessage `json:"items"`
}

// Renew asks the service for fresh download addresses of a chain's layers.
type Renew struct {
	RenewalKey string            `json:"renewalKey"`
	Layers     []json.RawMessage `json:"layers"`
	NeededBlob string            `json:"neededBlob"`
	Packs      map[string]string `json:"packs,omitempty"`
}

// Renewed answers Renew.
type Renewed struct {
	Addresses   map[string]RCBlobAddress `json:"addresses,omitempty"`
	Unavailable bool                     `json:"unavailable,omitempty"`
}

// RCBlobAddress is a presigned blob address.
type RCBlobAddress struct {
	URL           string `json:"url"`
	ExpiresAtUnix int64  `json:"expiresAtUnix,omitempty"`
}

// Lookup asks the service for the values of calls the engine's cache missed.
type Lookup struct {
	Misses []LookupMiss `json:"misses"`
}

// LookupMiss is one call the engine's cache missed.
type LookupMiss struct {
	Recipe string   `json:"recipe"`
	Extra  []string `json:"extra,omitempty"`
	Field  string   `json:"field,omitempty"`
	Self   string   `json:"self,omitempty"`
	Inputs []string `json:"inputs,omitempty"`
}

// LookedUp answers Lookup, one item per miss, in order.
type LookedUp struct {
	Misses []LookedUpMiss `json:"misses"`
}

// LookedUpMiss is what the service does for one miss.
type LookedUpMiss struct {
	Status string `json:"status"`
	Merge  uint64 `json:"merge,omitempty"`
	Parts  string `json:"parts,omitempty"`
}

// RemoteCacheError is the body of an error reply.
type RemoteCacheError struct {
	Message string `json:"message"`
}

// dagger-pack v1 format constants.
const (
	DaggerPackVersion         = "dagger-pack v1"
	DefaultPackMaxMemberBytes = 4 << 20 // 4 MiB
	MaxPackMembers            = 4096
	MaxPackBytes              = 64 << 20
	MaxPackHeaderBytes        = 512 << 10
)

// PackMember is one member slice inside a dagger-pack object.
type PackMember struct {
	Digest string `json:"digest"`
	Offset int64  `json:"offset,omitempty"` // absolute byte offset from the start of the pack object
	Size   int64  `json:"size"`
}

// PackManifest describes the members of one dagger-pack object.
type PackManifest struct {
	ID      PackID       `json:"id"`
	Members []PackMember `json:"members"`
	Total   int64        `json:"total"`
}

// PackID is a deterministic dagger-pack v1 identifier.
type PackID struct {
	Version string   `json:"version"`
	Members []string `json:"members"`
	Digest  string   `json:"digest"`
}

// PackHeader renders the dagger-pack v1 header for members: the magic line,
// the member count, then one "<digest> <size>" line per member in order. The
// pack's ID is the sha256 of exactly these bytes.
func PackHeader(members []PackMember) ([]byte, error) {
	if len(members) == 0 {
		return nil, fmt.Errorf("pack has no members")
	}
	if len(members) > MaxPackMembers {
		return nil, fmt.Errorf("pack has %d members, over %d", len(members), MaxPackMembers)
	}
	var b bytes.Buffer
	b.WriteString(DaggerPackVersion)
	b.WriteByte('\n')
	b.WriteString(strconv.Itoa(len(members)))
	b.WriteByte('\n')
	seen := make(map[string]bool, len(members))
	var total int64
	for _, m := range members {
		if !strings.HasPrefix(m.Digest, "sha256:") || len(m.Digest) != len("sha256:")+64 {
			return nil, fmt.Errorf("pack member %q is not sha256", m.Digest)
		}
		if seen[m.Digest] {
			return nil, fmt.Errorf("pack lists %s twice", m.Digest)
		}
		seen[m.Digest] = true
		if m.Size < 0 {
			return nil, fmt.Errorf("pack member %s has negative size", m.Digest)
		}
		if m.Size > MaxPackBytes-total {
			return nil, fmt.Errorf("pack is over %d bytes", MaxPackBytes)
		}
		total += m.Size
		b.WriteString(m.Digest)
		b.WriteByte(' ')
		b.WriteString(strconv.FormatInt(m.Size, 10))
		b.WriteByte('\n')
	}
	if b.Len() > MaxPackHeaderBytes {
		return nil, fmt.Errorf("pack header is %d bytes, over %d", b.Len(), MaxPackHeaderBytes)
	}
	if total > MaxPackBytes-int64(b.Len()) {
		return nil, fmt.Errorf("pack is over %d bytes", MaxPackBytes)
	}
	return b.Bytes(), nil
}

// PackHeaderLen returns the byte length PackHeader produces for members, so
// absolute member offsets can be derived without rendering the header twice.
func PackHeaderLen(members []PackMember) int64 {
	n := int64(len(DaggerPackVersion) + 1) // magic line
	n += int64(len(strconv.Itoa(len(members))) + 1)
	for _, m := range members {
		n += int64(len(m.Digest) + 1 + len(strconv.FormatInt(m.Size, 10)) + 1)
	}
	return n
}

// BuildPackID builds the dagger-pack v1 ID from its members: the sha256 digest
// of PackHeader(members). Members are listed in pack order.
func BuildPackID(members []PackMember) (PackID, error) {
	header, err := PackHeader(members)
	if err != nil {
		return PackID{}, err
	}
	sum := sha256.Sum256(header)
	digests := make([]string, len(members))
	for i, m := range members {
		digests[i] = m.Digest
	}
	return PackID{Version: DaggerPackVersion, Members: digests, Digest: fmt.Sprintf("sha256:%s", hex.EncodeToString(sum[:]))}, nil
}
