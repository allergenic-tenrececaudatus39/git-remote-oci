package helper

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// Packet-line framing, as used by git's wire protocol.
//
// Every packet is a four-digit hexadecimal length followed by that many bytes
// *including the four digits themselves*, so the shortest data packet is 0005.
// Three lengths are special and carry no payload:
//
//	0000  flush-pkt         end of a section or of the response
//	0001  delim-pkt         separates a command's capabilities from its arguments
//	0002  response-end-pkt  end of a stateless-rpc response
//
// See gitprotocol-common(5). This is the whole of the framing; everything above
// it — ls-refs, fetch, sideband — is text and bytes inside these packets.

const (
	// pktLenSize is the four hex digits every packet begins with.
	pktLenSize = 4
	// pktMaxPayload is the largest payload one packet may carry. The wire
	// format caps the total at 65520, which leaves 65516 after the header.
	pktMaxPayload = 65516
)

// pktKind distinguishes a data packet from the three special lengths.
type pktKind int

const (
	pktData pktKind = iota
	pktFlush
	pktDelim
	pktResponseEnd
)

func (k pktKind) String() string {
	switch k {
	case pktData:
		return "data"
	case pktFlush:
		return "flush-pkt"
	case pktDelim:
		return "delim-pkt"
	case pktResponseEnd:
		return "response-end-pkt"
	default:
		return "unknown"
	}
}

// errPktTooLong reports a payload that cannot be framed in one packet.
var errPktTooLong = errors.New("packet payload exceeds the pkt-line maximum")

// pktWriter frames writes as packet-lines.
type pktWriter struct {
	w io.Writer
}

func newPktWriter(w io.Writer) *pktWriter { return &pktWriter{w: w} }

// WriteData frames one payload. Payloads longer than a packet are the caller's
// problem to split, because where a split is legal depends on the payload:
// splitting a ref advertisement line would corrupt it, splitting packfile bytes
// inside a sideband channel is expected.
func (p *pktWriter) WriteData(payload []byte) error {
	if len(payload) > pktMaxPayload {
		return fmt.Errorf("%w: %d bytes", errPktTooLong, len(payload))
	}
	header := fmt.Sprintf("%04x", len(payload)+pktLenSize)
	if _, err := io.WriteString(p.w, header); err != nil {
		return err
	}
	_, err := p.w.Write(payload)
	return err
}

// WriteLine frames one text line, appending the trailing LF that git's
// text-mode packets carry.
func (p *pktWriter) WriteLine(format string, a ...any) error {
	return p.WriteData([]byte(fmt.Sprintf(format, a...) + "\n"))
}

func (p *pktWriter) Flush() error       { return p.writeSpecial("0000") }
func (p *pktWriter) Delim() error       { return p.writeSpecial("0001") }
func (p *pktWriter) ResponseEnd() error { return p.writeSpecial("0002") }

func (p *pktWriter) writeSpecial(s string) error {
	_, err := io.WriteString(p.w, s)
	return err
}

// pktReader reads packet-lines.
type pktReader struct {
	r *bufio.Reader
}

func newPktReader(r io.Reader) *pktReader {
	if br, ok := r.(*bufio.Reader); ok {
		return &pktReader{r: br}
	}
	return &pktReader{r: bufio.NewReader(r)}
}

// Read returns the next packet. A data packet's payload keeps whatever bytes it
// carried; callers wanting a text line should trim the trailing LF themselves,
// because a payload is not required to have one.
func (p *pktReader) Read() ([]byte, pktKind, error) {
	var header [pktLenSize]byte
	if _, err := io.ReadFull(p.r, header[:]); err != nil {
		// io.EOF here means the stream ended cleanly between packets, which is
		// meaningful to a caller; anything else is a truncated header.
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, pktData, fmt.Errorf("truncated packet header: %w", err)
		}
		return nil, pktData, err
	}

	var lenBytes [2]byte
	if _, err := hex.Decode(lenBytes[:], header[:]); err != nil {
		return nil, pktData, fmt.Errorf("malformed packet length %q: %w", header, err)
	}
	length := int(lenBytes[0])<<8 | int(lenBytes[1])

	switch length {
	case 0:
		return nil, pktFlush, nil
	case 1:
		return nil, pktDelim, nil
	case 2:
		return nil, pktResponseEnd, nil
	case 3:
		// 0003 would describe a packet shorter than its own header.
		return nil, pktData, fmt.Errorf("invalid packet length 0003")
	}

	payload := make([]byte, length-pktLenSize)
	if _, err := io.ReadFull(p.r, payload); err != nil {
		return nil, pktData, fmt.Errorf("truncated packet body (%d bytes): %w", length-pktLenSize, err)
	}
	return payload, pktData, nil
}

// ReadLine returns the next data packet as a string with its trailing LF
// removed, and reports the kind so a caller can tell "" from a flush.
func (p *pktReader) ReadLine() (string, pktKind, error) {
	payload, kind, err := p.Read()
	if err != nil || kind != pktData {
		return "", kind, err
	}
	s := string(payload)
	if n := len(s); n > 0 && s[n-1] == '\n' {
		s = s[:n-1]
	}
	return s, pktData, nil
}
