package oci_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// ociTagPattern is the tag grammar from the OCI distribution specification.
var ociTagPattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)

func TestEncodeRefTagKeepsSimpleBranchesReadable(t *testing.T) {
	tests := []struct {
		ref  string
		want string
	}{
		{"refs/heads/main", "main"},
		{"refs/heads/release-1.2", "release-1.2"},
		{"refs/tags/v1.0.0", "_t_v1.0.0"},
		{"refs/heads/feature/login", "feature_2flogin"},
		{"refs/heads/my_branch", "my__branch"},
		{"refs/notes/commits", "_r_notes_2fcommits"},
		{"HEAD", "_x_HEAD"},
	}

	for _, tt := range tests {
		if got := oci.EncodeRefTag(tt.ref); got != tt.want {
			t.Errorf("EncodeRefTag(%q) = %q, want %q", tt.ref, got, tt.want)
		}
	}
}

// TestEncodeRefTagDistinguishesPreviouslyCollidingRefs is the regression test
// for the collision bug: these pairs all mapped to a single tag and therefore
// overwrote each other's manifest.
func TestEncodeRefTagDistinguishesPreviouslyCollidingRefs(t *testing.T) {
	pairs := [][2]string{
		{"refs/heads/feature/foo", "refs/heads/feature-foo"},
		{"refs/heads/v1", "refs/tags/v1"},
		{"refs/heads/a/b/c", "refs/heads/a-b-c"},
		{"refs/heads/x_y", "refs/heads/x__y"},
		{"refs/tags/rel/1", "refs/tags/rel-1"},
	}

	for _, pair := range pairs {
		a, b := oci.EncodeRefTag(pair[0]), oci.EncodeRefTag(pair[1])
		if a == b {
			t.Errorf("%q and %q both encode to %q", pair[0], pair[1], a)
		}
	}
}

func TestEncodeRefTagProducesValidOCITags(t *testing.T) {
	refs := []string{
		"refs/heads/main",
		"refs/heads/-leading-dash",
		"refs/heads/.leading-dot",
		"refs/heads/feature/deeply/nested/branch",
		"refs/tags/v1.0.0-rc.1+build",
		"refs/heads/" + strings.Repeat("very-long-branch-name/", 20),
		"refs/heads/ünïcödé",
		"refs/heads/with space",
		"refs/heads/" + strings.Repeat("x", 500),
	}

	for _, ref := range refs {
		tag := oci.EncodeRefTag(ref)
		if tag == "" {
			t.Errorf("EncodeRefTag(%q) returned an empty tag", ref)
			continue
		}
		if !ociTagPattern.MatchString(tag) {
			t.Errorf("EncodeRefTag(%q) = %q, which is not a valid OCI tag", ref, tag)
		}
	}
}

// TestEncodeRefTagLongRefsStayDistinct: refs too long to encode are truncated
// and disambiguated with a digest of the full name.
func TestEncodeRefTagLongRefsStayDistinct(t *testing.T) {
	base := "refs/heads/" + strings.Repeat("segment/", 40)
	a := oci.EncodeRefTag(base + "one")
	b := oci.EncodeRefTag(base + "two")

	if a == b {
		t.Fatalf("two long refs sharing a prefix both encode to %q", a)
	}
	for _, tag := range []string{a, b} {
		if !ociTagPattern.MatchString(tag) {
			t.Errorf("truncated tag %q is not a valid OCI tag", tag)
		}
	}
}

func TestEncodeRefTagEmpty(t *testing.T) {
	if got := oci.EncodeRefTag(""); got != "" {
		t.Errorf("EncodeRefTag(\"\") = %q, want \"\"", got)
	}
}
