package gc_test

import (
	"testing"

	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// TestClassifyTag pins how garbage collection decides what a tag is for.
// Misclassifying a ref manifest as a prunable commit manifest would delete
// live refs, so this is the safety-critical part of the design.
func TestClassifyTag(t *testing.T) {
	tests := []struct {
		tag  string
		want oci.TagClass
	}{
		{"_refs", oci.TagClassMetadata},
		{"_index", oci.TagClassMetadata},
		{"_lfs_locks", oci.TagClassMetadata},
		{"_lock_main", oci.TagClassLock},
		{"_lock__t_v1.0.0", oci.TagClassLock},
		// Not a lock: a branch literally named "lock-main". While locks lived
		// outside the reserved namespace these were the same tag, and gc would
		// prune the branch as a released lock.
		{"lock-main", oci.TagClassRef},
		{"a1b2c3d4e5f60718293a4b5c6d7e8f901a2b3c4d", oci.TagClassCommit},
		{"main", oci.TagClassRef},
		{"_t_v1.0.0", oci.TagClassRef},
		{"feature_2flogin", oci.TagClassRef},
		// 39 and 41 hex characters are not commit ids.
		{"a1b2c3d4e5f60718293a4b5c6d7e8f901a2b3c4", oci.TagClassRef},
		{"a1b2c3d4e5f60718293a4b5c6d7e8f901a2b3c4dd", oci.TagClassRef},
	}

	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			if got := oci.ClassifyTag(tt.tag); got != tt.want {
				t.Errorf("ClassifyTag(%q) = %v, want %v", tt.tag, got, tt.want)
			}
		})
	}
}

// TestClassifyTagNeverPrunesARefManifest is the property that matters: the tag
// a ref is actually published under must never be classified as a prunable
// commit manifest, or garbage collection would delete a live ref.
//
// Note this must use RefManifestTag, not EncodeRefTag. A branch named after a
// commit id encodes to a bare 40-hex tag; RefManifestTag is what prefixes it so
// it cannot be confused with a commit manifest.
func TestClassifyTagNeverPrunesARefManifest(t *testing.T) {
	refs := []string{
		"refs/heads/main",
		"refs/heads/feature/login",
		"refs/tags/v1.0.0",
		"refs/heads/my_branch",
		"refs/notes/commits",
		"refs/heads/a1b2c3d4e5f60718293a4b5c6d7e8f901a2b3c4d",
	}
	for _, ref := range refs {
		tag := oci.RefManifestTag(ref)
		if got := oci.ClassifyTag(tag); got == oci.TagClassCommit {
			t.Errorf("ref %q encodes to %q, which gc would prune as a commit manifest", ref, tag)
		}
	}
}
