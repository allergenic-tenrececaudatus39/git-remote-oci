package oci_test

import (
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// The pack index answers the one question the rest of the format cannot:
// which packfile holds this object. Everything else is about commits, so
// without it a partial clone's lazy fetch has to download history and look.
//
// It is only ever used to skip work, which decides how it must fail: a wrong
// "yes" costs a pack fetched for nothing, and a wrong "no" tells a client its
// object does not exist. The tests below are mostly about that asymmetry.

// ids builds index entries from bare object ids, for tests about which objects
// an index lists rather than how large they are.
func ids(n ...string) []oci.PackIndexEntry {
	entries := make([]oci.PackIndexEntry, 0, len(n))
	for i, oid := range n {
		entries = append(entries, oci.PackIndexEntry{OID: oid, Size: int64(i)})
	}
	return entries
}

// oidsOf is the same list as bare strings, for the lookup side.
func oidsOf(n ...string) []string { return n }

// A line is the object id, a space, and the size as sixteen zero-padded hex
// digits: 40 + 1 + 16 for SHA-1. The width is fixed so that a reader can seek
// to a multiple of it and binary-search rather than parse.
const sha1IndexLineLen = 40 + 1 + 16

func TestEncodePackIndexIsSortedAndFixedWidth(t *testing.T) {
	a := strings.Repeat("a", 40)
	b := strings.Repeat("b", 40)
	c := strings.Repeat("c", 40)

	// ids() gives each entry a distinct size by position, so this also pins
	// that the size travels with its own id through the sort rather than
	// staying where it was.
	got := string(oci.EncodePackIndex(ids(c, a, b)))
	want := a + " 0000000000000001\n" +
		b + " 0000000000000002\n" +
		c + " 0000000000000000\n"
	if got != want {
		t.Errorf("index =\n%q\nwant\n%q", got, want)
	}
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if len(line) != sha1IndexLineLen {
			t.Errorf("line %q is %d chars; the stride must be uniform", line, len(line))
		}
	}
}

func TestEncodePackIndexNormalisesAndDropsNonIds(t *testing.T) {
	upper := strings.Repeat("AB", 20)
	got := string(oci.EncodePackIndex(ids(upper, "not-an-object-id", "")))
	if want := strings.Repeat("ab", 20) + " 0000000000000000\n"; got != want {
		t.Errorf("index = %q, want the id lowercased and the rest dropped", got)
	}
}

// TestEncodePackIndexDropsUnrepresentableSizes: a negative size cannot be
// written in the fixed field and cannot be true of an object. Recording zero
// instead would be a number a reader acts on.
func TestEncodePackIndexDropsUnrepresentableSizes(t *testing.T) {
	good := strings.Repeat("a", 40)
	bad := strings.Repeat("b", 40)

	index := oci.EncodePackIndex([]oci.PackIndexEntry{
		{OID: good, Size: 7},
		{OID: bad, Size: -1},
	})
	if !oci.PackIndexContains(index, oidsOf(good)) {
		t.Error("the valid entry was dropped")
	}
	if oci.PackIndexContains(index, oidsOf(bad)) {
		t.Error("an entry with a negative size was published")
	}
}

// TestPackIndexSize is what object-info reads.
func TestPackIndexSize(t *testing.T) {
	a := strings.Repeat("a", 40)
	b := strings.Repeat("b", 64)

	index := oci.EncodePackIndex([]oci.PackIndexEntry{{OID: a, Size: 4321}})
	if size, ok := oci.PackIndexSize(index, a); !ok || size != 4321 {
		t.Errorf("PackIndexSize = %d, %v; want 4321, true", size, ok)
	}
	if _, ok := oci.PackIndexSize(index, strings.Repeat("c", 40)); ok {
		t.Error("an absent object reported a size")
	}

	// SHA-256 widths carry sizes too; the stride distinguishes them.
	wide := oci.EncodePackIndex([]oci.PackIndexEntry{{OID: b, Size: 99}})
	if size, ok := oci.PackIndexSize(wide, b); !ok || size != 99 {
		t.Errorf("PackIndexSize on a SHA-256 index = %d, %v; want 99, true", size, ok)
	}

	// A v1 index -- ids and nothing else -- must report "no size", not zero.
	// Zero is a number a caller would act on.
	legacy := []byte(a + "\n")
	if size, ok := oci.PackIndexSize(legacy, a); ok {
		t.Errorf("a v1 index answered with size %d; it records none", size)
	}
	if !oci.PackIndexContains(legacy, oidsOf(a)) {
		t.Error("a v1 index is still a usable membership index and must be read")
	}
}

func TestPackIndexContains(t *testing.T) {
	a := strings.Repeat("a", 40)
	m := strings.Repeat("5", 40)
	z := strings.Repeat("f", 40)
	index := oci.EncodePackIndex(ids(a, m, z))

	for _, tc := range []struct {
		name string
		oids []string
		want bool
	}{
		{"the first entry", oidsOf(a), true},
		{"one in the middle", oidsOf(m), true},
		{"the last entry", oidsOf(z), true},
		{"absent", oidsOf(strings.Repeat("b", 40)), false},
		// Any, not all: one wanted object in the pack is reason enough to
		// fetch it.
		{"one of several present", oidsOf(strings.Repeat("b", 40), m), true},
		{"none of several present", oidsOf(strings.Repeat("b", 40), strings.Repeat("c", 40)), false},
		{"nothing asked for", nil, false},
		// A SHA-256 id cannot be in a SHA-1 repository's pack, and must not
		// be compared against one at the wrong stride.
		{"a wider id against a narrow index", oidsOf(strings.Repeat("a", 64)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := oci.PackIndexContains(index, tc.oids); got != tc.want {
				t.Errorf("PackIndexContains(%v) = %v, want %v", tc.oids, got, tc.want)
			}
		})
	}
}

// TestPackIndexContainsWorksForSHA256: the width is read from the blob rather
// than configured, the same rule the rest of the format follows.
func TestPackIndexContainsWorksForSHA256(t *testing.T) {
	a := strings.Repeat("a", 64)
	b := strings.Repeat("b", 64)
	index := oci.EncodePackIndex(ids(b, a))

	if !oci.PackIndexContains(index, oidsOf(a)) {
		t.Error("a SHA-256 id was not found in a SHA-256 index")
	}
	if oci.PackIndexContains(index, oidsOf(strings.Repeat("c", 64))) {
		t.Error("an absent SHA-256 id was reported present")
	}
}

// TestPackIndexContainsFailsTowardsFetching is the asymmetry that matters. An
// index that cannot be parsed must not be read as "this pack is empty": the
// cost of a wrong yes is a wasted download, and the cost of a wrong no is
// telling a client its object does not exist.
func TestPackIndexContainsFailsTowardsFetching(t *testing.T) {
	want := oidsOf(strings.Repeat("a", 40))
	for _, tc := range []struct {
		name  string
		index []byte
	}{
		{"truncated", []byte("abc")},
		{"no newline at the expected stride", []byte(strings.Repeat("a", 40))},
		{"not hex at all", []byte("this is not an index\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !oci.PackIndexContains(tc.index, want) {
				t.Error("an unreadable index was treated as proof the object is absent")
			}
		})
	}

	// An empty index is different: it was written, and it says the pack holds
	// nothing. That is a real answer.
	if oci.PackIndexContains(nil, want) {
		t.Error("an empty index should report the object absent, not present")
	}
}
