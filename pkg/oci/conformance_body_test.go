package oci_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// conformance_test.go covers FORMAT.md's Conformance section: eight numbered
// clauses. This file covers the other ninety normative statements in the body,
// which is where the last four defects actually lived — each one a requirement
// the document stated plainly and nothing checked.
//
// Organised by section, named for the requirement. Where a statement is not
// testable from here, it is listed at the bottom with the reason, so that the
// difference between "checked" and "not worth checking here" stays visible
// rather than being inferred from silence.

// ociTagGrammar is the tag rule from §3, written out rather than imported, so
// this asserts the specification and not the implementation's opinion of it.
var ociTagGrammar = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)

func bodyFixture(t *testing.T) (*registrytest.Registry, *oci.Client, string) {
	t.Helper()
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	_, tip := registrytest.SeedRepository(t, client, 2)
	return reg, registrytest.Client(t, ts), tip
}

func manifestOf(t *testing.T, reg *registrytest.Registry, tag string) ocispec.Manifest {
	t.Helper()
	var m ocispec.Manifest
	if err := json.Unmarshal(reg.RawManifest(t, tag), &m); err != nil {
		t.Fatalf("manifest %s: %v", tag, err)
	}
	return m
}

// --- §2 Object model -------------------------------------------------------

// "Object ids appearing in tags, annotations and index blobs MUST be lowercase
// hexadecimal. A reader MAY accept uppercase on input; a writer MUST NOT emit
// it."
func TestBodyObjectIdsArePublishedLowercase(t *testing.T) {
	reg, client, _ := bodyFixture(t)

	refs, err := client.FetchRichRefIndex(context.Background())
	if err != nil {
		t.Fatalf("FetchRichRefIndex: %v", err)
	}
	for name, entry := range refs {
		if entry.SHA != strings.ToLower(entry.SHA) {
			t.Errorf("%s is published as %q, which is not lowercase", name, entry.SHA)
		}
	}
	for _, tag := range reg.Tags() {
		if oci.ClassifyTag(tag) == oci.TagClassCommit && tag != strings.ToLower(tag) {
			t.Errorf("commit tag %q is not lowercase", tag)
		}
	}
}

// "A repository MUST use one hash algorithm throughout ... A reader derives the
// algorithm from the width of the ids it finds, and MUST NOT rely on any
// recorded field, because there is none."
func TestBodyHashAlgorithmIsNotRecordedAnywhere(t *testing.T) {
	reg, _, _ := bodyFixture(t)

	// If any manifest recorded the algorithm, the two could disagree. The check
	// is that no such field exists to disagree with.
	for _, tag := range reg.Tags() {
		raw := string(reg.RawManifest(t, tag))
		for _, forbidden := range []string{"object-format", "objectformat", "hash-algorithm", "sha256\":", "algo\":"} {
			if strings.Contains(raw, forbidden) {
				t.Errorf("%s appears to record the hash algorithm (%q); it must be derived from the ids",
					tag, forbidden)
			}
		}
	}
}

// --- §3 Ref names to tags --------------------------------------------------

// "An OCI tag MUST match [a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}."
func TestBodyEveryEncodedTagMatchesTheOCIGrammar(t *testing.T) {
	for _, refName := range []string{
		"refs/heads/main", "refs/heads/feature/login", "refs/heads/my_branch",
		"refs/tags/v1.0.0", "refs/notes/commits", "HEAD",
		"refs/heads/ünïcøde", "refs/heads/a b", "refs/heads/x~y^z:w?*[",
		"refs/heads/" + strings.Repeat("deep/", 40) + "end",
		"refs/heads/" + strings.Repeat("q", 300),
		"refs/heads/\x01control",
	} {
		tag := oci.EncodeRefTag(refName)
		if tag == "" {
			continue
		}
		if !ociTagGrammar.MatchString(tag) {
			t.Errorf("%q encodes to %q, which is not a legal OCI tag", refName, tag)
		}
		if len(tag) > 128 {
			t.Errorf("%q encodes to %d bytes, past the 128 limit", refName, len(tag))
		}
	}
}

// "A ref whose encoding exceeds 128 bytes MUST be stored as _h_<prefix>-<hash>."
func TestBodyOverLongRefsUseTheHashedForm(t *testing.T) {
	long := "refs/heads/" + strings.Repeat("q", 300)
	tag := oci.EncodeRefTag(long)

	if !strings.HasPrefix(tag, "_h_") {
		t.Errorf("an over-long ref encoded to %q, without the _h_ marker that keeps it "+
			"distinguishable from a short ref spelled like the truncation", tag)
	}
	if !ociTagGrammar.MatchString(tag) {
		t.Errorf("the hashed form %q is not a legal OCI tag", tag)
	}
	// Still injective: two long refs sharing a prefix must not share a tag.
	other := oci.EncodeRefTag("refs/heads/" + strings.Repeat("q", 301))
	if tag == other {
		t.Error("two distinct over-long refs encode to the same tag")
	}
}

// "A ref whose encoded tag is 40 hex characters ... MUST be published under
// ref-<tag> instead." Otherwise it lands in the commit-manifest namespace.
func TestBodyRefsThatLookLikeCommitIdsAreMoved(t *testing.T) {
	hexish := "refs/heads/" + strings.Repeat("a", 40)
	tag := oci.EncodeRefTag(hexish)
	if tag == "" {
		t.Fatal("no tag produced")
	}
	refTag := oci.RefManifestTag(hexish)
	if oci.ClassifyTag(refTag) == oci.TagClassCommit {
		t.Errorf("the ref manifest tag %q is in the commit namespace; publishing it would "+
			"collide with the manifest for that commit id", refTag)
	}
}

// --- §4 Commit manifests ---------------------------------------------------

// "Layer 0 MUST be the packfile" (§4.1), and the config blob is a valid OCI
// image config with platform unknown/unknown (§4).
func TestBodyCommitManifestShape(t *testing.T) {
	reg, _, tip := bodyFixture(t)
	m := manifestOf(t, reg, tip)

	if len(m.Layers) == 0 {
		t.Fatal("the commit manifest has no layers")
	}
	if !strings.HasPrefix(m.Layers[0].MediaType, "application/vnd.git.repository.packfile") {
		t.Errorf("layer 0 is %q, not the packfile", m.Layers[0].MediaType)
	}
	if m.Config.MediaType != ocispec.MediaTypeImageConfig {
		t.Errorf("config media type is %q", m.Config.MediaType)
	}

	var cfg ocispec.Image
	blob := reg.BlobBytes(m.Config.Digest.String())
	if err := json.Unmarshal(blob, &cfg); err != nil {
		t.Fatalf("the config blob is not an OCI image config: %v", err)
	}
	if cfg.OS != "unknown" || cfg.Architecture != "unknown" {
		t.Errorf("config platform is %s/%s, want unknown/unknown", cfg.OS, cfg.Architecture)
	}
}

// "The io.git-remote-oci.pack-bases annotation is REQUIRED on every commit
// manifest and every ref manifest." The ref manifest half is the one easy to
// forget, because it is a copy of the commit manifest and copies drift.
func TestBodyPackBasesIsPresentOnBothManifestKinds(t *testing.T) {
	reg, _, tip := bodyFixture(t)

	for _, tag := range []string{tip, oci.EncodeRefTag("refs/heads/main")} {
		m := manifestOf(t, reg, tag)
		if _, err := oci.ParsePackBases(m.Annotations); err != nil {
			t.Errorf("%s: %v", tag, err)
		}
	}
}

// --- §4.4 Pack index -------------------------------------------------------

// "a reader MUST still accept it [v1]."
//
// The writer half -- "a writer MUST publish v2" -- is not asserted here. The
// seeded fixture is built by the registry client directly and never goes
// through the push path that attaches an index, so a check here would be
// testing the fixture. It is covered where a real push happens:
// pkg/helper/regression_packindex_test.go for the layer, pkg/gc/run_test.go for
// the sizes in it.
func TestBodyPackIndexVersions(t *testing.T) {
	oid := strings.Repeat("a", 40)
	if !oci.PackIndexContains([]byte(oid+"\n"), []string{oid}) {
		t.Error("a v1 index was not accepted")
	}
	if _, has := oci.PackIndexSize([]byte(oid+"\n"), oid); has {
		t.Error("a v1 index reported a size it does not record")
	}
}

// "Object ids MUST be lowercase hex and MUST be unique", and a writer MUST omit
// the layer rather than emit one with mixed widths or an unrepresentable size.
func TestBodyPackIndexWriterRules(t *testing.T) {
	a := strings.Repeat("a", 40)
	wide := strings.Repeat("b", 64)

	if got := oci.EncodePackIndex([]oci.PackIndexEntry{{OID: a, Size: 1}, {OID: wide, Size: 2}}); got != nil {
		t.Errorf("an index mixing id widths was emitted (%d bytes); it must be omitted", len(got))
	}

	dup := oci.EncodePackIndex([]oci.PackIndexEntry{{OID: a, Size: 1}, {OID: a, Size: 2}})
	if lines := strings.Count(string(dup), "\n"); lines != 1 {
		t.Errorf("a repeated id produced %d lines; ids must be unique", lines)
	}
	if upper := oci.EncodePackIndex([]oci.PackIndexEntry{{OID: strings.ToUpper(a), Size: 1}}); !strings.Contains(string(upper), a) {
		t.Errorf("an uppercase id was not normalised: %q", upper)
	}
}

// "A reader MUST reject a size it cannot represent rather than wrapping it."
func TestBodyPackIndexRejectsAnUnrepresentableSize(t *testing.T) {
	oid := strings.Repeat("a", 40)
	// ffffffffffffffff is 2^64-1, which no int64 can hold.
	blob := []byte(oid + " ffffffffffffffff\n")

	size, has := oci.PackIndexSize(blob, oid)
	if has {
		t.Errorf("a size of 2^64-1 was reported as %d; it cannot be represented and must be unknown", size)
	}
	if size < 0 {
		t.Errorf("a negative size (%d) escaped, which a caller would report to git", size)
	}
}

// --- §5 Ref manifests ------------------------------------------------------

// "It MUST carry the same layers as the commit manifest it points at", and
// "A writer MUST NOT write org.opencontainers.image.signature."
func TestBodyRefManifestMirrorsTheCommitManifest(t *testing.T) {
	reg, _, tip := bodyFixture(t)

	commit := manifestOf(t, reg, tip)
	ref := manifestOf(t, reg, oci.EncodeRefTag("refs/heads/main"))

	if len(ref.Layers) != len(commit.Layers) {
		t.Errorf("the ref manifest has %d layers, the commit manifest %d", len(ref.Layers), len(commit.Layers))
	}
	for i := range ref.Layers {
		if i < len(commit.Layers) && ref.Layers[i].Digest != commit.Layers[i].Digest {
			t.Errorf("layer %d differs between the ref and commit manifests", i)
		}
	}
	for _, m := range []ocispec.Manifest{commit, ref} {
		if v, present := m.Annotations["org.opencontainers.image.signature"]; present {
			t.Errorf("a signature annotation was published (%q); it records nothing verifiable", v)
		}
	}
}

// --- §6 `_refs` ------------------------------------------------------------

// "An entry MUST be a JSON object; a reader MUST reject a bare string."
func TestBodyRefIndexRejectsABareStringEntry(t *testing.T) {
	var entry oci.RefEntry
	err := json.Unmarshal([]byte(`"a1b2c3d4e5f60718293a4b5c6d7e8f901a2b3c4d"`), &entry)
	if err == nil && entry.SHA != "" {
		t.Errorf("a bare string was accepted as a ref entry (sha=%q)", entry.SHA)
	}
}

// --- §7 `_index` -----------------------------------------------------------

// "The index MUST be annotated io.git-remote-oci.type: repository-index", each
// entry MUST carry platform unknown/unknown and io.git-remote-oci.ref, and a
// reader MUST NOT infer a ref name from the short name.
func TestBodyImageIndexShape(t *testing.T) {
	reg, _, _ := bodyFixture(t)

	var idx ocispec.Index
	if err := json.Unmarshal(reg.RawManifest(t, oci.TagOCIIndex), &idx); err != nil {
		t.Fatalf("_index: %v", err)
	}
	if idx.Annotations["io.git-remote-oci.type"] != "repository-index" {
		t.Errorf("_index is not marked as a repository index; a tool cannot tell it from an "+
			"ordinary multi-platform image index (annotations: %v)", idx.Annotations)
	}
	if len(idx.Manifests) == 0 {
		t.Fatal("_index lists no manifests")
	}
	for _, child := range idx.Manifests {
		if child.Platform == nil || child.Platform.OS != "unknown" || child.Platform.Architecture != "unknown" {
			t.Errorf("child %s has platform %v, want unknown/unknown", child.Digest, child.Platform)
		}
		if child.Annotations["io.git-remote-oci.ref"] == "" {
			t.Errorf("child %s carries no full ref name, so a reader would have to guess from the tag",
				child.Digest)
		}
		if child.Annotations["org.opencontainers.image.ref.name"] == "" {
			t.Errorf("child %s carries no encoded tag", child.Digest)
		}
	}
}

// --- §9 Locks --------------------------------------------------------------

// "A reader MUST honour expiry; an expired lock is not a lock."
func TestBodyAnExpiredLockIsNotALock(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	registrytest.SeedRepository(t, client, 1)

	// Acquired normally and then aged, rather than asked for with a short TTL.
	// A zero or negative TTL is read as "unspecified" and clamped to the
	// default, and one short enough to expire on its own expires during
	// acquisition -- the read-back step finds it gone and reports a lost race,
	// which is the protocol working, not the thing under test.
	//
	// Without expiry being honoured, a client that died mid-push would wedge
	// the ref until someone ran break-lock.
	if _, err := client.AcquireRefLock(context.Background(), "refs/heads/main", time.Minute); err != nil {
		t.Fatalf("AcquireRefLock: %v", err)
	}
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if err := reg.SetManifestAnnotation(oci.LockTag("refs/heads/main"), oci.AnnotationLockExpiresAt, past); err != nil {
		t.Fatalf("could not age the lock: %v", err)
	}
	locked, info, err := registrytest.Client(t, ts).IsLocked(context.Background(), "refs/heads/main")
	if err != nil {
		t.Fatalf("IsLocked: %v", err)
	}
	if locked {
		t.Errorf("an expired lock is still reported as held: %+v", info)
	}
}

// "Releasing MUST write a tombstone with owner `released` rather than deleting."
func TestBodyReleasingALockWritesATombstone(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	registrytest.SeedRepository(t, client, 1)

	if _, err := client.AcquireRefLock(context.Background(), "refs/heads/main", time.Minute); err != nil {
		t.Fatalf("AcquireRefLock: %v", err)
	}
	if err := client.ReleaseRefLock(context.Background(), "refs/heads/main"); err != nil {
		t.Fatalf("ReleaseRefLock: %v", err)
	}

	// Deletion may be refused by the registry, so release must not depend on
	// it: the manifest stays, marked released.
	tag := oci.LockTag("refs/heads/main")
	raw := string(reg.RawManifest(t, tag))
	if !strings.Contains(raw, "released") {
		t.Errorf("the released lock %s is not marked released: %s", tag, raw)
	}
	locked, _, err := registrytest.Client(t, ts).IsLocked(context.Background(), "refs/heads/main")
	if err != nil || locked {
		t.Errorf("a released lock still reads as held (locked=%v, err=%v)", locked, err)
	}
}

// --- §11 Changing the format -----------------------------------------------

// The version moves when a change would make an older reader misread a
// repository, and not otherwise. That is a judgement no test can make.
//
// What a test can do is stop the two records of it drifting apart. The number
// lives in the code and is documented at the top of FORMAT.md, and a bump that
// updates one and not the other leaves a specification describing a version
// nobody writes — which is worse than either value on its own, because both
// look authoritative.
func TestBodyFormatVersionMatchesTheSpecification(t *testing.T) {
	spec, err := os.ReadFile(filepath.Join("..", "..", "FORMAT.md"))
	if err != nil {
		t.Fatalf("read FORMAT.md: %v", err)
	}

	declared := regexp.MustCompile(`\*\*Specification version ([0-9]+)\*\*`).FindSubmatch(spec)
	if declared == nil {
		t.Fatal("FORMAT.md no longer declares a specification version in the form " +
			"**Specification version N**; this test reads it from there")
	}
	if got := string(declared[1]); got != oci.FormatVersion {
		t.Errorf("FORMAT.md declares version %q, the code writes %q. A bump has to move both, "+
			"or the specification describes a version nothing produces.", got, oci.FormatVersion)
	}
}

// --- not tested here -------------------------------------------------------
//
// The statements below are normative and deliberately have no test in this
// file. Listed so that "untested" is a decision on the record rather than
// something to be inferred:
//
//   §4.2  the invariant that a manifest plus its bases holds every reachable
//         object — a property of a real push against real history, covered by
//         the end-to-end suite against two registry implementations.
//   §4.2  "fetch every base before importing the packfile that depends on it"
//         and cycle detection — behaviour of the fetch path, covered in
//         pkg/helper and by `fsck`.
//   §4.5  LFS content verified against its oid before storage — covered by
//         pkg/lfs, including a fuzz target.
//   §6    "_refs updates under the index lock, as a read-modify-write against
//         what is published" — covered by the gc concurrency tests, which is
//         where getting it wrong actually cost something.
//   §8    "a deletion that fails for any other reason MUST be reported" —
//         needs a registry that fails deletion in a specific way; covered
//         against a real one in the end-to-end suite.
//   §2.1  "a writer MUST label a layer with the compression it applied" —
//         covered by the compression round-trip tests in pkg/oci.
