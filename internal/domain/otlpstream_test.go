package domain

import (
	"bytes"
	"testing"
)

func TestOTLPFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	frames := []OTLPFrame{
		{Type: FrameData, Seq: 1, Payload: []byte("hello")},
		{Type: FrameData, Seq: 2},
		{Type: FrameTerminal, Seq: 3},
	}
	for _, f := range frames {
		if err := WriteOTLPFrame(&buf, f); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	for _, want := range frames {
		got, err := ReadOTLPFrame(&buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if got.Type != want.Type || got.Seq != want.Seq || !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
}

func TestOTLPFrameShort(t *testing.T) {
	if _, err := ReadOTLPFrame(bytes.NewReader([]byte("short"))); err != ErrShortFrame {
		t.Fatalf("err = %v, want ErrShortFrame", err)
	}
}
