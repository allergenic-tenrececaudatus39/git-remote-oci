package oci

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// hexObjectID is what a git object id looks like, written out independently of
// the code under test.
var hexObjectID = regexp.MustCompile(`^([0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

// The pack index and the pack chain are both parsed from blobs the registry
// served, which is the same trust boundary the existing fuzz targets guard:
// a repository can be written by anyone who can push to it, and a reader has
// no way to know a layer was produced by this code.
//
// What is being looked for is not "does it reject bad input" -- both are
// designed to shrug and fall back -- but that no input makes them panic, and
// that neither ever answers in the direction that loses data. For the index
// that means never claiming an object is absent on the strength of bytes it
// could not read; for the chain, never handing out an identifier that is about
// to be used as a tag.

// FuzzPackIndexContains: arbitrary index bytes against arbitrary queries.
//
// The slicing here is index[i*stride : i*stride+stride-1] with n computed by
// integer division, which is correct by argument. This is the version that does
// not depend on my argument being right.
func FuzzPackIndexContains(f *testing.F) {
	sha1 := strings.Repeat("a", 40)
	sha256 := strings.Repeat("b", 64)

	f.Add([]byte(sha1+"\n"), sha1)
	f.Add([]byte(sha256+"\n"), sha256)
	f.Add(EncodePackIndex([]string{sha1, strings.Repeat("c", 40)}), strings.Repeat("c", 40))
	f.Add([]byte(""), sha1)
	f.Add([]byte("\n"), sha1)
	f.Add([]byte(sha1), sha1)                // no trailing newline
	f.Add([]byte(sha1+"\n"+"short\n"), sha1) // ragged
	f.Add([]byte("\x00\x00\x00\n"), sha1)
	f.Add([]byte(strings.Repeat("a\n", 100)), "a")

	f.Fuzz(func(t *testing.T, index []byte, oid string) {
		got := PackIndexContains(index, []string{oid})

		// The one guarantee that matters: a blob this cannot make sense of is
		// never read as proof the object is absent. Saying "absent" wrongly
		// tells a client its object does not exist; saying "present" wrongly
		// costs a download.
		if _, readable := packIndexStride(index); !readable && len(index) > 0 && !got {
			t.Errorf("unreadable index of %d bytes reported %q absent", len(index), oid)
		}

		// And the answer must not depend on being asked alone or in company.
		if alone := PackIndexContains(index, []string{oid}); alone != got {
			t.Errorf("PackIndexContains is not deterministic for %q", oid)
		}
		if len(oid) > 0 {
			together := PackIndexContains(index, []string{oid, strings.Repeat("f", 40)})
			if got && !together {
				t.Errorf("a hit for %q disappeared when asked alongside another id", oid)
			}
		}
	})
}

// FuzzEncodePackIndexRoundTrip: whatever survives encoding must be findable,
// and the stride must stay uniform however strange the input.
func FuzzEncodePackIndexRoundTrip(f *testing.F) {
	f.Add(strings.Repeat("a", 40), strings.Repeat("b", 40))
	f.Add(strings.Repeat("A", 64), "")
	f.Add("not-an-id", strings.Repeat("9", 40))
	f.Add("", "")

	f.Fuzz(func(t *testing.T, a, b string) {
		index := EncodePackIndex([]string{a, b})
		if len(index) == 0 {
			return
		}

		stride, ok := packIndexStride(index)
		if !ok {
			t.Fatalf("EncodePackIndex produced a blob it cannot read back: %q", index)
		}
		if len(index)%stride != 0 {
			t.Fatalf("ragged index: %d bytes at stride %d", len(index), stride)
		}
		// Only a well-formed id has to survive; anything else is legitimately
		// dropped to keep the stride uniform. The pattern is spelled out here
		// rather than reusing isObjectID, so this asserts the contract instead
		// of agreeing with the implementation about what the contract is.
		for _, id := range []string{a, b} {
			if !hexObjectID.MatchString(id) {
				continue
			}
			if !PackIndexContains(index, []string{strings.ToLower(id)}) {
				t.Errorf("%q is a well-formed object id but cannot be found after encoding", id)
			}
		}
	})
}

// FuzzSanitisePackChain: every identifier that comes out has to be safe to use
// as a tag, whatever went in. This is the check that the chain path applies the
// validation ParsePackBases applies to the same data arriving as an annotation.
func FuzzSanitisePackChain(f *testing.F) {
	sha := strings.Repeat("a", 40)
	f.Add(`{"` + sha + `":[]}`)
	f.Add(`{"` + sha + `":["` + strings.Repeat("b", 64) + `"]}`)
	f.Add(`{"../../etc/passwd":["` + sha + `"]}`)
	f.Add(`{"` + sha + `":["../.."]}`)
	f.Add(`{}`)
	f.Add(`null`)
	f.Add(`{"":[""]}`)

	f.Fuzz(func(t *testing.T, raw string) {
		var chain map[string][]string
		if err := json.Unmarshal([]byte(raw), &chain); err != nil {
			return
		}
		clean := sanitisePackChain(chain)

		for sha, bases := range clean {
			if !isObjectID(sha) {
				t.Errorf("sanitisePackChain kept %q as a key; it will be requested as a tag", sha)
			}
			for _, base := range bases {
				if !isObjectID(base) {
					t.Errorf("sanitisePackChain kept %q as a base of %q", base, sha)
				}
			}
		}
		// Sanitising twice must change nothing, or the boundary is not one.
		if len(sanitisePackChain(clean)) != len(clean) {
			t.Error("sanitisePackChain is not idempotent")
		}
	})
}
