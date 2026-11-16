package service

import (
	"fmt"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// PackBuilder accumulates member digests and enforces dagger-pack v1 limits.
type PackBuilder struct {
	members []string
	bytes   int
}

// NewPackBuilder returns an empty builder.
func NewPackBuilder() *PackBuilder { return &PackBuilder{} }

// Add appends a member digest. It returns false when the pack is full.
func (b *PackBuilder) Add(digest string, size int) bool {
	if len(b.members) >= domain.MaxPackMembers || b.bytes+size > domain.MaxPackBytes {
		return false
	}
	b.members = append(b.members, digest)
	b.bytes += size
	return true
}

// ID returns the deterministic pack ID.
func (b *PackBuilder) ID() domain.PackID { return domain.BuildPackID(b.members) }

// Len returns the number of members.
func (b *PackBuilder) Len() int { return len(b.members) }

// Bytes returns the accumulated payload size.
func (b *PackBuilder) Bytes() int { return b.bytes }

// PackHeader renders the dagger-pack v1 header line.
func PackHeader(id domain.PackID) string {
	return fmt.Sprintf("%s %s", id.Version, id.Digest)
}
