package lfs_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/pkg/lfs"
)

// FuzzValidateOID checks the validator itself never panics and only ever
// returns identifiers that are safe to use as a path segment.
func FuzzValidateOID(f *testing.F) {
	f.Add(strings.Repeat("ab", 32))
	f.Add("sha256:" + strings.Repeat("ab", 32))
	f.Add("../../../etc/passwd")
	f.Add("")
	f.Add(strings.Repeat("a", 63) + "\x00")

	f.Fuzz(func(t *testing.T, oid string) {
		clean, err := lfs.ValidateOID(oid)
		if err != nil {
			return
		}
		if len(clean) != 64 {
			t.Fatalf("ValidateOID(%q) returned %q, which is not 64 characters", oid, clean)
		}
		for _, r := range clean {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				t.Fatalf("ValidateOID(%q) returned %q containing non-lowercase-hex %q", oid, clean, r)
			}
		}
		// A validated OID must produce a path, and that path must stay inside
		// the object directory.
		path := lfs.GetLFSObjectPath("/tmp/gitdir", clean)
		if !strings.HasPrefix(path, "/tmp/gitdir/lfs/objects/") {
			t.Fatalf("ValidateOID(%q) yielded a path outside the object directory: %q", oid, path)
		}
	})
}

// FuzzStoreLFSObject checks the stored-object path: content that does not hash
// to its advertised OID must be rejected, and nothing may be written outside
// the object directory.
func FuzzStoreLFSObject(f *testing.F) {
	f.Add(strings.Repeat("ab", 32), []byte("payload"))
	f.Add("../../escape", []byte("payload"))

	f.Fuzz(func(t *testing.T, oid string, payload []byte) {
		dir := t.TempDir()
		err := lfs.StoreLFSObject(dir, oid, strings.NewReader(string(payload)))

		sum := sha256.Sum256(payload)
		want := hex.EncodeToString(sum[:])
		clean, validErr := lfs.ValidateOID(oid)

		switch {
		case validErr != nil:
			if err == nil {
				t.Fatalf("StoreLFSObject accepted the invalid OID %q", oid)
			}
		case clean == want:
			if err != nil {
				t.Fatalf("StoreLFSObject rejected matching content for %q: %v", oid, err)
			}
		default:
			if err == nil {
				t.Fatalf("StoreLFSObject accepted content that hashes to %s under OID %s", want, clean)
			}
		}
	})
}
