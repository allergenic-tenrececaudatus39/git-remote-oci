package helper

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// The framing is the layer everything else in protocol v2 sits on, and a bug
// here does not produce a wrong answer — it desynchronises the stream and
// produces a hang or a nonsense error somewhere far away. It is worth pinning
// against the byte sequences in gitprotocol-common(5) rather than against
// itself.

func TestPktWriterFramesKnownBytes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(*pktWriter) error
		want  string
	}{
		// The four hex digits count themselves: "a\n" is two bytes, so 0006.
		{"a line", func(p *pktWriter) error { return p.WriteLine("a") }, "0006a\n"},
		{"empty line", func(p *pktWriter) error { return p.WriteLine("") }, "0005\n"},
		{"raw data", func(p *pktWriter) error { return p.WriteData([]byte("hi")) }, "0006hi"},
		{"empty data", func(p *pktWriter) error { return p.WriteData(nil) }, "0004"},
		{"flush", (*pktWriter).Flush, "0000"},
		{"delim", (*pktWriter).Delim, "0001"},
		{"response end", (*pktWriter).ResponseEnd, "0002"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tc.write(newPktWriter(&buf)); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got := buf.String(); got != tc.want {
				t.Errorf("framed %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPktWriterRefusesAnOversizedPayload(t *testing.T) {
	var buf bytes.Buffer
	err := newPktWriter(&buf).WriteData(make([]byte, pktMaxPayload+1))
	if !errors.Is(err, errPktTooLong) {
		t.Fatalf("error = %v, want errPktTooLong", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a refused write still emitted %d bytes, desynchronising the stream", buf.Len())
	}
}

func TestPktWriterAcceptsTheLargestLegalPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := newPktWriter(&buf).WriteData(make([]byte, pktMaxPayload)); err != nil {
		t.Fatalf("largest legal payload rejected: %v", err)
	}
	if got := buf.Len(); got != pktMaxPayload+pktLenSize {
		t.Errorf("wrote %d bytes, want %d", got, pktMaxPayload+pktLenSize)
	}
	if got := buf.String()[:4]; got != "fff0" {
		t.Errorf("length header %q, want fff0", got)
	}
}

func TestPktReaderReadsWhatTheWriterWrote(t *testing.T) {
	var buf bytes.Buffer
	w := newPktWriter(&buf)
	for _, line := range []string{"ls-refs=unborn", "object-format=sha1"} {
		if err := w.WriteLine("%s", line); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := w.Delim(); err != nil {
		t.Fatalf("delim: %v", err)
	}
	if err := w.WriteLine("peel"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	r := newPktReader(&buf)
	for _, want := range []struct {
		text string
		kind pktKind
	}{
		{"ls-refs=unborn", pktData},
		{"object-format=sha1", pktData},
		{"", pktDelim},
		{"peel", pktData},
		{"", pktFlush},
	} {
		got, kind, err := r.ReadLine()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if kind != want.kind || got != want.text {
			t.Fatalf("read (%q, %v), want (%q, %v)", got, kind, want.text, want.kind)
		}
	}
	if _, _, err := r.Read(); !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF after the last packet, got %v", err)
	}
}

// TestPktReaderDistinguishesEmptyFromFlush is the distinction a naive reader
// gets wrong: "0005\n" is a line containing nothing, "0000" is the end of a
// section, and treating them alike ends a section early.
func TestPktReaderDistinguishesEmptyFromFlush(t *testing.T) {
	r := newPktReader(strings.NewReader("0005\n0000"))

	text, kind, err := r.ReadLine()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if kind != pktData || text != "" {
		t.Errorf("first packet is (%q, %v), want an empty data packet", text, kind)
	}

	if _, kind, err = r.ReadLine(); err != nil || kind != pktFlush {
		t.Errorf("second packet is (%v, %v), want a flush", kind, err)
	}
}

func TestPktReaderRejectsMalformedInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"non-hex length", "zzzz"},
		{"length 0003", "0003"},
		{"truncated header", "00"},
		{"truncated body", "0010short"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := newPktReader(strings.NewReader(tc.in)).Read()
			if err == nil || errors.Is(err, io.EOF) {
				t.Errorf("malformed input accepted, err = %v", err)
			}
		})
	}
}

// TestPktReaderReportsCleanEOF: a stream that ends between packets is how a
// conversation finishes, and must be distinguishable from a truncated one.
func TestPktReaderReportsCleanEOF(t *testing.T) {
	_, _, err := newPktReader(strings.NewReader("")).Read()
	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

// TestPktKindString covers the names that appear in error messages when the
// framing goes wrong. A desynchronised stream is diagnosed by reading those
// messages, so "delim-pkt" saying "data" would send the reader after the wrong
// bug entirely.
func TestPktKindString(t *testing.T) {
	for _, tc := range []struct {
		kind pktKind
		want string
	}{
		{pktData, "data"},
		{pktFlush, "flush-pkt"},
		{pktDelim, "delim-pkt"},
		{pktResponseEnd, "response-end-pkt"},
		// A value from outside the set has to render as something rather than
		// empty: the whole point of the string is to appear in a message.
		{pktKind(99), "unknown"},
	} {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("pktKind(%d).String() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}
