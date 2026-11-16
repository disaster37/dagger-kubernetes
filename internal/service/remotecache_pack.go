package service

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"

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

// PackMemberPayload is one member's digest and bytes.
type PackMemberPayload struct {
	Digest string
	Data   []byte
}

// PackMemberSpec is one member's digest and payload size. It is used when the
// payload bytes are not available locally (e.g. presigning an engine-side pack
// upload) but the absolute byte offsets must still be derived.
type PackMemberSpec struct {
	Digest string
	Size   int64
}

// PackHeaderLen returns the byte length of the dagger-pack v1 header for the
// given pack ID and members, so absolute member offsets can be derived without
// the payload bytes. It must stay byte-for-byte identical to EncodePack.
func PackHeaderLen(id domain.PackID, members []PackMemberSpec) int64 {
	n := int64(len(fmt.Sprintf("%s %s\n", id.Version, id.Digest)))
	n += int64(len(fmt.Sprintf("%d\n", len(members))))
	var rel int64
	for _, m := range members {
		n += int64(len(fmt.Sprintf("%s %d %d\n", m.Digest, rel, m.Size)))
		rel += m.Size
	}
	return n + 1 // trailing blank line
}

// EncodePack assembles a dagger-pack v1 object and its manifest. Absolute byte
// offsets are computed so any member is directly addressable via HTTP Range.
func EncodePack(members []PackMemberPayload) ([]byte, domain.PackManifest, error) {
	if len(members) > domain.MaxPackMembers {
		return nil, domain.PackManifest{}, fmt.Errorf("pack exceeds %d members", domain.MaxPackMembers)
	}
	digests := make([]string, 0, len(members))
	for _, m := range members {
		digests = append(digests, m.Digest)
	}
	id := domain.BuildPackID(digests)

	var header bytes.Buffer
	fmt.Fprintf(&header, "%s %s\n", id.Version, id.Digest)
	fmt.Fprintf(&header, "%d\n", len(members))
	relOffset := int64(0)
	for _, m := range members {
		fmt.Fprintf(&header, "%s %d %d\n", m.Digest, relOffset, len(m.Data))
		relOffset += int64(len(m.Data))
	}
	header.WriteString("\n")
	headerLen := int64(header.Len())

	var body bytes.Buffer
	body.Write(header.Bytes())
	manifest := domain.PackManifest{ID: id, Members: make([]domain.PackMember, 0, len(members))}
	offset := headerLen
	for _, m := range members {
		manifest.Members = append(manifest.Members, domain.PackMember{
			Digest: m.Digest,
			Offset: offset,
			Size:   int64(len(m.Data)),
		})
		offset += int64(len(m.Data))
		body.Write(m.Data)
	}
	manifest.Total = int64(body.Len())
	return body.Bytes(), manifest, nil
}

// DecodePackHeader parses a dagger-pack v1 header and returns the manifest plus
// the absolute byte offset where member payloads begin.
func DecodePackHeader(r io.Reader) (domain.PackManifest, int64, error) {
	br := bufio.NewReader(r)
	first, err := br.ReadString('\n')
	if err != nil {
		return domain.PackManifest{}, 0, fmt.Errorf("read pack header: %w", err)
	}
	fields := strings.Fields(strings.TrimSpace(first))
	if len(fields) != 3 || fields[0] != "dagger-pack" || fields[1] != "v1" {
		return domain.PackManifest{}, 0, fmt.Errorf("invalid pack header")
	}
	manifest := domain.PackManifest{ID: domain.PackID{Version: domain.DaggerPackVersion, Digest: fields[2]}}
	countLine, err := br.ReadString('\n')
	if err != nil {
		return domain.PackManifest{}, 0, fmt.Errorf("read pack member count: %w", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(countLine))
	if err != nil || count < 0 || count > domain.MaxPackMembers {
		return domain.PackManifest{}, 0, fmt.Errorf("invalid pack member count")
	}
	headerLen := int64(len(first) + len(countLine))
	manifest.Members = make([]domain.PackMember, 0, count)
	for i := 0; i < count; i++ {
		line, err := br.ReadString('\n')
		if err != nil {
			return domain.PackManifest{}, 0, fmt.Errorf("read pack member: %w", err)
		}
		headerLen += int64(len(line))
		parts := strings.Fields(strings.TrimSpace(line))
		if len(parts) != 3 {
			return domain.PackManifest{}, 0, fmt.Errorf("invalid pack member line")
		}
		rel, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return domain.PackManifest{}, 0, fmt.Errorf("invalid pack member offset")
		}
		size, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return domain.PackManifest{}, 0, fmt.Errorf("invalid pack member size")
		}
		manifest.Members = append(manifest.Members, domain.PackMember{Digest: parts[0], Offset: rel, Size: size})
	}
	blank, err := br.ReadString('\n')
	if err != nil {
		return domain.PackManifest{}, 0, fmt.Errorf("read pack header terminator: %w", err)
	}
	headerLen += int64(len(blank))
	for i := range manifest.Members {
		manifest.Members[i].Offset += headerLen
		manifest.Total += manifest.Members[i].Size
	}
	return manifest, headerLen, nil
}
