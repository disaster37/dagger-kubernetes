package domain

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// OTLPStreamContentType is the framed binary OTLP read-back media type.
const OTLPStreamContentType = "application/vnd.dagger.otlp.stream"

// OTLPStreamHeaderSize is the fixed 16-byte frame header size.
const OTLPStreamHeaderSize = 16

// FrameType is the 4-byte frame magic.
type FrameType [4]byte

var (
	// FrameData carries an OTLP protobuf chunk (or an empty heartbeat).
	FrameData = FrameType{'D', 'T', 'P', 0x01}
	// FrameTerminal marks the end of a completed trace stream.
	FrameTerminal = FrameType{'D', 'T', 'E', 0x01}
	// FrameError carries a terminal error message.
	FrameError = FrameType{'D', 'T', 'X', 0x01}
)

// OTLPFrame is one framed chunk. Header layout (big endian):
//
//	magic[4] | sequence[8] | payload length[4]
type OTLPFrame struct {
	Type    FrameType
	Seq     uint64
	Payload []byte
}

// ErrShortFrame is returned when a frame header or payload is truncated.
var ErrShortFrame = errors.New("short otlpstream frame")

// WriteOTLPFrame encodes f to w.
func WriteOTLPFrame(w io.Writer, f OTLPFrame) error {
	if len(f.Payload) > int(^uint32(0)) {
		return fmt.Errorf("otlpstream payload too large: %d", len(f.Payload))
	}
	var hdr [OTLPStreamHeaderSize]byte
	copy(hdr[0:4], f.Type[:])
	binary.BigEndian.PutUint64(hdr[4:12], f.Seq)
	binary.BigEndian.PutUint32(hdr[12:16], uint32(len(f.Payload))) // #nosec G115 -- length checked against MaxUint32 above
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("write otlpstream header: %w", err)
	}
	if len(f.Payload) > 0 {
		if _, err := w.Write(f.Payload); err != nil {
			return fmt.Errorf("write otlpstream payload: %w", err)
		}
	}
	return nil
}

// ReadOTLPFrame decodes one frame from r.
func ReadOTLPFrame(r io.Reader) (OTLPFrame, error) {
	var hdr [OTLPStreamHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return OTLPFrame{}, ErrShortFrame
		}
		return OTLPFrame{}, fmt.Errorf("read otlpstream header: %w", err)
	}
	var f OTLPFrame
	copy(f.Type[:], hdr[0:4])
	f.Seq = binary.BigEndian.Uint64(hdr[4:12])
	n := binary.BigEndian.Uint32(hdr[12:16])
	if n > 0 {
		f.Payload = make([]byte, n)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			return OTLPFrame{}, ErrShortFrame
		}
	}
	return f, nil
}

// OTLPChunk is one archived OTLP payload with its immutable sequence number.
type OTLPChunk struct {
	Seq     uint64
	Payload []byte
}

// OTLPArchive persists raw OTLP protobuf chunks per (traceID, signal).
type OTLPArchive interface {
	Append(ctx context.Context, traceID, signal string, seq uint64, payload []byte) error
	// ChunksAfter returns archived chunks with Seq >= fromSeq in ascending order.
	ChunksAfter(ctx context.Context, traceID, signal string, fromSeq uint64) ([]OTLPChunk, error)
	DeleteTrace(ctx context.Context, traceID string) error
}
