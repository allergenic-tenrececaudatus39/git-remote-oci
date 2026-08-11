package oci_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// FORMAT.md has a Conformance section: four numbered clauses for a writer and
// four for a reader. Nothing checked the code against them, and the gap showed
// — reader clause 3 says "validate every registry-supplied object id before
// using it as a tag", the pack-bases and pack-chain paths did, and the `_refs`
// index, which every operation reads first, did not. It had been possible to
// forge a `list` line for as long as the claim had been in the documentation.
//
// So the clauses get tests, named after themselves. These are not a substitute
// for the behavioural tests elsewhere; they are the answer to "does this
// implementation still satisfy the thing the specification says it does",
// asked in a form that fails when the answer changes.
//
// Writer clause 1 (the pack-bases invariant on every push) has no test here.
// It is a property of a whole push against real history, and the end-to-end
// suite exercises it against two registry implementations; restating it here
// with a mock would test the mock. That absence is deliberate and is the reason
// this comment says so.

// conformanceFixture is a seeded repository and a client that reads it.
func conformanceFixture(t *testing.T) (*registrytest.Registry, *oci.Client, string) {
	t.Helper()
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	_, tip := registrytest.SeedRepository(t, client, 2)
	// A second client, so nothing under test is answered from the caches the
	// seeding pushes filled.
	return reg, registrytest.Client(t, ts), tip
}

// --- writer clause 2: publish `_refs` and `_index` together ----------------

func TestConformanceWriterPublishesBothIndexesTogether(t *testing.T) {
	reg, client, tip := conformanceFixture(t)

	refs, err := client.FetchRichRefIndex(context.Background())
	if err != nil {
		t.Fatalf("_refs: %v", err)
	}
	mirror, err := client.FetchOCIImageIndexRefs(context.Background(), "")
	if err != nil {
		t.Fatalf("_index: %v", err)
	}

	if len(mirror) != len(refs) {
		t.Errorf("_index lists %d refs, _refs lists %d", len(mirror), len(refs))
	}
	for name, entry := range refs {
		if mirror[name].SHA != entry.SHA {
			t.Errorf("%s: _index says %q, _refs says %q", name, mirror[name].SHA, entry.SHA)
		}
	}
	if refs["refs/heads/main"].SHA != tip {
		t.Errorf("_refs does not point at the tip")
	}

	// "with the same format version": both carry it, because either may be the
	// one a reader reaches.
	for _, tag := range []string{oci.TagRefIndex, oci.TagOCIIndex} {
		if v := annotationOf(t, reg, tag, oci.AnnotationFormatVersion); v == "" {
			t.Errorf("%s carries no format version", tag)
		}
	}
}

// --- writer clause 3: never publish a ref under a reserved tag -------------

func TestConformanceWriterNeverUsesAReservedTag(t *testing.T) {
	// The encoding is the whole defence, so it is exercised directly rather
	// than by pushing refs and hoping one of them was awkward.
	for _, refName := range []string{
		"refs/heads/_refs", "refs/heads/_index", "refs/heads/_lfs_locks",
		"refs/heads/_", "refs/heads/__", "refs/heads/_t_x", "refs/heads/_h_x",
		"refs/tags/_refs",
		// Named to collide with the lock namespace. This one found a real bug:
		// while locks lived under "lock-", which is not reserved, the branch
		// "lock-main" and the lock on "main" shared a tag.
		"refs/heads/lock-main", "refs/heads/_lock_main", "refs/tags/lock-v1",
	} {
		tag := oci.EncodeRefTag(refName)
		if tag == "" {
			continue
		}
		for _, reserved := range []string{oci.TagRefIndex, oci.TagOCIIndex, "_lfs_locks"} {
			if tag == reserved {
				t.Errorf("%s encodes to the reserved tag %q", refName, tag)
			}
		}
		if strings.HasPrefix(tag, oci.LockTagPrefix) {
			t.Errorf("%s encodes to %q, which is inside the lock namespace: pushing it would "+
				"overwrite a lock, and gc would prune it as one", refName, tag)
		}
	}

	// And the converse: no ref's lock can land on another ref's manifest.
	for _, refName := range []string{"refs/heads/main", "refs/heads/lock-main", "refs/tags/v1"} {
		if lockTag := oci.LockTag(refName); !strings.HasPrefix(lockTag, oci.LockTagPrefix) {
			t.Errorf("the lock for %s is tagged %q, outside the reserved namespace", refName, lockTag)
		}
	}
}

// --- writer clause 4: an injective ref-name mapping ------------------------

func TestConformanceWriterMappingIsInjective(t *testing.T) {
	// Names chosen to collide under a careless encoding: the marker prefixes,
	// the escape character, the separator, and the two namespaces that share a
	// short name.
	names := []string{
		"refs/heads/main", "refs/tags/main", "refs/notes/main", "HEAD",
		"refs/heads/a_b", "refs/heads/a__b", "refs/heads/a/b", "refs/heads/a_2fb",
		"refs/heads/_t_x", "refs/tags/x", "refs/heads/x",
		"refs/heads/" + strings.Repeat("q", 200), "refs/heads/" + strings.Repeat("q", 201),
		"refs/heads/feature/login", "refs/heads/feature_2flogin",
	}

	seen := make(map[string]string, len(names))
	for _, name := range names {
		tag := oci.EncodeRefTag(name)
		if tag == "" {
			t.Errorf("%q has no tag at all", name)
			continue
		}
		if other, clash := seen[tag]; clash {
			t.Errorf("%q and %q both encode to %q; one would overwrite the other", other, name, tag)
		}
		seen[tag] = name
	}
}

// --- reader clause 1: check the format version first -----------------------

func TestConformanceReaderChecksTheFormatVersion(t *testing.T) {
	for _, tag := range []string{oci.TagRefIndex, oci.TagOCIIndex} {
		t.Run(tag, func(t *testing.T) {
			reg, client, _ := conformanceFixture(t)
			setAnnotation(t, reg, tag, oci.AnnotationFormatVersion, "99")

			var err error
			if tag == oci.TagRefIndex {
				_, err = client.FetchRichRefIndex(context.Background())
			} else {
				_, err = client.FetchOCIImageIndexRefs(context.Background(), "")
			}
			if err == nil {
				t.Fatalf("%s with an unimplemented format version was read anyway", tag)
			}
			if !strings.Contains(err.Error(), "99") && !strings.Contains(strings.ToLower(err.Error()), "version") {
				t.Errorf("the error does not name the version as the problem: %v", err)
			}
		})
	}
}

// --- reader clause 2: pack-bases, transitively and loudly ------------------

func TestConformanceReaderRejectsBadPackBases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bases string
	}{
		{"absent", ""},
		{"empty", "   "},
		{"not an object id", "not-a-commit"},
		{"a plausible-looking short id", "abc123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			annotations := map[string]string{}
			if tc.bases != "" {
				annotations[oci.AnnotationGitPackBases] = tc.bases
			}
			if _, err := oci.ParsePackBases(annotations); err == nil {
				t.Errorf("pack-bases %q was accepted; absent or malformed must be an error, "+
					"never read as 'none'", tc.bases)
			}
		})
	}

	// And "none" is the one spelling that means self-contained.
	bases, err := oci.ParsePackBases(map[string]string{oci.AnnotationGitPackBases: oci.PackBasesNone})
	if err != nil || len(bases) != 0 {
		t.Errorf("PackBasesNone = %v, %v; want empty, nil", bases, err)
	}
}

// --- reader clause 3: validate every id before it becomes a tag ------------

// This is the clause the implementation was failing. Each route by which a
// registry-supplied object id reaches a tag gets a case, because they are
// separate parsers of the same fact and fixing one has never fixed the others.
func TestConformanceReaderValidatesObjectIdsFromEveryRoute(t *testing.T) {
	const hostile = "../../../etc/passwd"

	t.Run("the pack-bases annotation", func(t *testing.T) {
		if _, err := oci.ParsePackBases(map[string]string{
			oci.AnnotationGitPackBases: hostile,
		}); err == nil {
			t.Error("a pack-bases entry that is not an object id was accepted")
		}
	})

	t.Run("the ref index", func(t *testing.T) {
		reg, client, tip := conformanceFixture(t)
		poison(t, reg, map[string]oci.RefEntry{
			"refs/heads/main": {SHA: tip},
			"refs/heads/bad":  {SHA: hostile},
		})
		refs, err := client.FetchRichRefIndex(context.Background())
		if err != nil {
			t.Fatalf("FetchRichRefIndex: %v", err)
		}
		if _, present := refs["refs/heads/bad"]; present {
			t.Error("a ref whose SHA is not an object id survived the read")
		}
		if _, present := refs["refs/heads/main"]; !present {
			t.Error("the valid ref was dropped along with the bad one")
		}
	})

	t.Run("the ref index tag object", func(t *testing.T) {
		reg, client, tip := conformanceFixture(t)
		poison(t, reg, map[string]oci.RefEntry{
			"refs/tags/bad": {SHA: tip, TagObject: hostile},
		})
		refs, err := client.FetchRichRefIndex(context.Background())
		if err != nil {
			t.Fatalf("FetchRichRefIndex: %v", err)
		}
		if _, present := refs["refs/tags/bad"]; present {
			t.Error("a ref whose tag object is not an object id survived the read")
		}
	})

	t.Run("the ref index ref name", func(t *testing.T) {
		reg, client, tip := conformanceFixture(t)
		poison(t, reg, map[string]oci.RefEntry{
			"refs/heads/main":  {SHA: tip},
			"refs/heads/x\ny":  {SHA: tip},
			"refs/heads/a b":   {SHA: tip},
			"refs/heads/tab\t": {SHA: tip},
		})
		refs, err := client.FetchRichRefIndex(context.Background())
		if err != nil {
			t.Fatalf("FetchRichRefIndex: %v", err)
		}
		for name := range refs {
			if strings.ContainsAny(name, " \t\n\r") {
				t.Errorf("%q survived the read; it cannot be a ref name and reaches stdout", name)
			}
		}
	})
}

// --- reader clause 4: every optional layer is absent-safe ------------------

// Ignoring an optional layer must cost performance and nothing else, which
// means each one has to be removable from a published repository without
// changing any answer.
func TestConformanceOptionalLayersAreAbsentSafe(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mediaType string
	}{
		{"the pack index", oci.MediaTypeGitPackIndexV2},
		{"the pack chain", oci.MediaTypePackChain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, client, tip := conformanceFixture(t)

			before, err := client.FetchRichRefIndex(context.Background())
			if err != nil {
				t.Fatalf("FetchRichRefIndex: %v", err)
			}
			stripLayers(t, reg, tc.mediaType)

			// A fresh client, or the answer comes from a cache filled while the
			// layer was still there.
			after, err := registrytest.Client(t, reg.LastServer()).FetchRichRefIndex(context.Background())
			if err != nil {
				t.Fatalf("reading a repository without %s failed: %v", tc.name, err)
			}
			if len(after) != len(before) || after["refs/heads/main"].SHA != tip {
				t.Errorf("removing %s changed what the repository says: %v vs %v", tc.name, after, before)
			}
		})
	}
}

// --- helpers ---------------------------------------------------------------

func poison(t *testing.T, reg *registrytest.Registry, refs map[string]oci.RefEntry) {
	t.Helper()
	client := registrytest.Client(t, reg.LastServer())
	if err := client.PushRichRefIndex(context.Background(), refs, nil); err != nil {
		t.Fatalf("could not publish the index: %v", err)
	}
}

func annotationOf(t *testing.T, reg *registrytest.Registry, tag, key string) string {
	t.Helper()
	var m ocispec.Manifest
	raw := reg.RawManifest(t, tag)
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest %s: %v", tag, err)
	}
	if m.Annotations != nil {
		return m.Annotations[key]
	}
	// An image index carries its annotations on a different struct.
	var idx ocispec.Index
	if err := json.Unmarshal(raw, &idx); err == nil && idx.Annotations != nil {
		return idx.Annotations[key]
	}
	return ""
}

func setAnnotation(t *testing.T, reg *registrytest.Registry, tag, key, value string) {
	t.Helper()
	if err := reg.SetManifestAnnotation(tag, key, value); err != nil {
		t.Fatalf("could not set %s on %s: %v", key, tag, err)
	}
}

func stripLayers(t *testing.T, reg *registrytest.Registry, mediaType string) {
	t.Helper()
	if err := reg.StripLayers(mediaType); err != nil {
		t.Fatalf("could not strip %s: %v", mediaType, err)
	}
}

// --- §8: a reader must skip tombstones ------------------------------------

// A registry that refuses manifest deletion gets a tombstone instead, and a
// reader that does not skip it resurrects the ref. Tag enumeration skipped
// them; the `_index` mirror did not, and that is the path taken when `_refs`
// cannot be read — so the fallback was the one that could bring a deleted ref
// back.
func TestConformanceReaderSkipsTombstonesInTheMirror(t *testing.T) {
	reg, client, tip := conformanceFixture(t)

	if err := reg.MarkIndexEntryDeleted("refs/heads/main"); err != nil {
		t.Fatalf("could not tombstone the mirror entry: %v", err)
	}

	refs, err := client.FetchOCIImageIndexRefs(context.Background(), "")
	if err != nil {
		t.Fatalf("FetchOCIImageIndexRefs: %v", err)
	}
	if entry, present := refs["refs/heads/main"]; present {
		t.Errorf("a tombstoned ref was resurrected by the _index fallback (%s)", entry.SHA)
	}
	_ = tip
}
