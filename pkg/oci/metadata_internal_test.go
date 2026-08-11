package oci

import (
	"bytes"
	"strings"
	"testing"
)

// Manifests, index blobs and lock lists are read whole into memory, because
// they are JSON and there is nothing useful to stream. Their sizes come from
// the registry, so an unbounded read is an invitation: declare a very large
// blob, serve it, and the client dies holding it.
//
// The newest readers here bounded themselves and the older ones did not, which
// is the usual shape of this — the rule existed in some places and had never
// been written down. readMetadataBlob is that rule.

func TestReadMetadataBlobBoundsByDeclaredSize(t *testing.T) {
	body := strings.Repeat("x", 100)

	// Exactly the declared size is fine.
	got, err := readMetadataBlob(strings.NewReader(body), 100, "test")
	if err != nil || len(got) != 100 {
		t.Errorf("reading exactly the declared size failed: %d bytes, %v", len(got), err)
	}

	// More than declared is a registry not keeping its word, and the extra is
	// exactly what a caller must not be handed.
	if _, err := readMetadataBlob(strings.NewReader(body+"more"), 100, "test"); err == nil {
		t.Error("a body larger than its declared size was accepted")
	}

	// Less than declared is fine: short is not dangerous, and the JSON parse
	// downstream will say so if it matters.
	if got, err := readMetadataBlob(strings.NewReader("xx"), 100, "test"); err != nil || len(got) != 2 {
		t.Errorf("a short body was rejected: %d bytes, %v", len(got), err)
	}
}

func TestReadMetadataBlobRefusesAnAbsurdDeclaration(t *testing.T) {
	// Refused before a byte is read: the point is not to discover the size by
	// receiving it.
	var served bytes.Buffer
	_, err := readMetadataBlob(&served, maxMetadataBytes+1, "the _refs index blob")
	if err == nil {
		t.Fatal("a declaration above the cap was accepted")
	}
	if !strings.Contains(err.Error(), "_refs index blob") {
		t.Errorf("the error does not say what was too large: %v", err)
	}
}

// TestReadMetadataBlobBoundsWithoutADeclaration: FetchReference hands back a
// stream before any size is known, so the absolute cap has to hold on its own.
func TestReadMetadataBlobBoundsWithoutADeclaration(t *testing.T) {
	// A reader that never ends, which is what a hostile registry streaming
	// junk looks like from here.
	endless := endlessReader{}
	got, err := readMetadataBlob(endless, 0, "a manifest")
	if err == nil {
		t.Fatalf("an endless stream was read to completion (%d bytes)", len(got))
	}
	if int64(len(got)) > maxMetadataBytes {
		t.Errorf("read %d bytes before giving up, past the %d cap", len(got), maxMetadataBytes)
	}
}

type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
