package oci

import (
	"regexp"
	"strings"
	"testing"
)

// ociTagPattern is the tag grammar from the OCI distribution specification.
// The external test file keeps its own copy; these are different packages.
var ociTagPattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)

func TestEncodeRefTagRoundTrips(t *testing.T) {
	refs := []string{
		"refs/heads/main",
		"refs/heads/feature/login",
		"refs/heads/my_branch",
		"refs/heads/-dash",
		"refs/heads/.dot",
		"refs/tags/v1.0.0",
		"refs/tags/nested/tag",
		"refs/notes/commits",
		"refs/heads/ünïcödé",
		"HEAD",
	}

	for _, ref := range refs {
		tag := EncodeRefTag(ref)
		got, err := decodeRefTag(tag)
		if err != nil {
			t.Errorf("decodeRefTag(%q) from ref %q: %v", tag, ref, err)
			continue
		}
		if got != ref {
			t.Errorf("round trip of %q via %q gave %q", ref, tag, got)
		}
	}
}

// FuzzEncodeRefTag asserts the two properties the encoding exists to provide:
// every output is a legal OCI tag, and the mapping is reversible - which
// implies it is injective, so two refs can never share a manifest.
func FuzzEncodeRefTag(f *testing.F) {
	seeds := []string{
		"refs/heads/main",
		"refs/heads/feature/login",
		"refs/tags/v1.0.0",
		"refs/heads/_underscore",
		"refs/heads/-dash",
		"refs/heads/..",
		"refs/heads/ünïcödé",
		"refs/heads/with space",
		"HEAD",
		"refs/",
		"",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, ref string) {
		tag := EncodeRefTag(ref)
		if ref == "" {
			if tag != "" {
				t.Fatalf("EncodeRefTag(%q) = %q, want an empty tag", ref, tag)
			}
			return
		}
		if tag == "" {
			// Only refs with no representable short name may encode to "".
			return
		}

		if !ociTagPattern.MatchString(tag) {
			t.Fatalf("EncodeRefTag(%q) = %q, which is not a valid OCI tag", ref, tag)
		}

		got, err := decodeRefTag(tag)
		if err != nil {
			// Only a truncated tag may refuse to decode, and truncation is
			// lossy by construction: the digest suffix keeps distinct refs
			// distinct without preserving the name.
			if !strings.HasPrefix(tag, "_h_") {
				t.Fatalf("decodeRefTag(%q) from ref %q failed: %v", tag, ref, err)
			}
			return
		}

		// Anything that decodes must decode back to exactly the ref it came
		// from. A truncated tag reaching this point would mean a long ref and
		// some short ref share a tag, which is the collision this encoding
		// exists to prevent.
		if got != ref {
			t.Fatalf("round trip of %q via %q gave %q", ref, tag, got)
		}
	})
}

// TestTruncatedTagsCannotCollideWithShortRefs is the regression test for a
// collision the fuzzer found in the first version of this encoding.
//
// A truncated tag ends in "-<digest>", which is also perfectly ordinary
// content. Without a reserved marker, decoding a truncated tag yielded a
// plausible short ref name that re-encoded to the *same* tag — so a long ref and
// that short ref shared one manifest, which is exactly what this encoding exists
// to prevent.
func TestTruncatedTagsCannotCollideWithShortRefs(t *testing.T) {
	longRef := "refs/heads/" + strings.Repeat("segment/", 40) + "tip"
	longTag := EncodeRefTag(longRef)

	if !strings.HasPrefix(longTag, "_h_") {
		t.Fatalf("expected a truncated tag to be marked, got %q", longTag)
	}
	if _, err := decodeRefTag(longTag); err == nil {
		t.Errorf("a truncated tag must not claim to decode: %q", longTag)
	}

	// The short ref spelled exactly like the truncated tag's payload must get a
	// different tag.
	shortRef := "refs/heads/" + strings.TrimPrefix(longTag, "_h_")
	if shortTag := EncodeRefTag(shortRef); shortTag == longTag {
		t.Errorf("long ref %q and short ref %q both encode to %q", longRef, shortRef, longTag)
	}
}
