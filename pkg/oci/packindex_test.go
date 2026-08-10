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

func ids(n ...string) []string { return n }

func TestEncodePackIndexIsSortedAndFixedWidth(t *testing.T) {
	a := strings.Repeat("a", 40)
	b := strings.Repeat("b", 40)
	c := strings.Repeat("c", 40)

	got := string(oci.EncodePackIndex(ids(c, a, b)))
	if want := a + "\n" + b + "\n" + c + "\n"; got != want {
		t.Errorf("index = %q, want it sorted", got)
	}
	// Fixed width is what lets a reader binary-search it rather than parse it.
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if len(line) != 40 {
			t.Errorf("line %q is %d chars; the stride must be uniform", line, len(line))
		}
	}
}

func TestEncodePackIndexNormalisesAndDropsNonIds(t *testing.T) {
	upper := strings.Repeat("AB", 20)
	got := string(oci.EncodePackIndex(ids(upper, "not-an-object-id", "")))
	if want := strings.Repeat("ab", 20) + "\n"; got != want {
		t.Errorf("index = %q, want the id lowercased and the rest dropped", got)
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
		{"the first entry", ids(a), true},
		{"one in the middle", ids(m), true},
		{"the last entry", ids(z), true},
		{"absent", ids(strings.Repeat("b", 40)), false},
		// Any, not all: one wanted object in the pack is reason enough to
		// fetch it.
		{"one of several present", ids(strings.Repeat("b", 40), m), true},
		{"none of several present", ids(strings.Repeat("b", 40), strings.Repeat("c", 40)), false},
		{"nothing asked for", nil, false},
		// A SHA-256 id cannot be in a SHA-1 repository's pack, and must not
		// be compared against one at the wrong stride.
		{"a wider id against a narrow index", ids(strings.Repeat("a", 64)), false},
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

	if !oci.PackIndexContains(index, ids(a)) {
		t.Error("a SHA-256 id was not found in a SHA-256 index")
	}
	if oci.PackIndexContains(index, ids(strings.Repeat("c", 64))) {
		t.Error("an absent SHA-256 id was reported present")
	}
}

// TestPackIndexContainsFailsTowardsFetching is the asymmetry that matters. An
// index that cannot be parsed must not be read as "this pack is empty": the
// cost of a wrong yes is a wasted download, and the cost of a wrong no is
// telling a client its object does not exist.
func TestPackIndexContainsFailsTowardsFetching(t *testing.T) {
	want := ids(strings.Repeat("a", 40))
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
