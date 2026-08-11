package cli_test

import (
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// fsck's own logic — checkRef and walkPackBases — had no test. The tool whose
// job is telling you whether a repository is still fetchable was itself
// unverified, which is the worst place for that to be true: a broken fsck
// reports a broken repository as healthy, and the failure only surfaces the
// next time somebody clones.

const mainTag = "main" // oci.EncodeRefTag("refs/heads/main")

func seeded(t *testing.T) (*registrytest.Registry, string, string) {
	t.Helper()

	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	_, tip := registrytest.SeedRepository(t, client, 3)
	return reg, registrytest.URL(ts), tip
}

func TestFsckPassesOnAHealthyRepository(t *testing.T) {
	_, url, _ := seeded(t)

	stdout, stderr, err := runCLI(t, "fsck", url)
	if err != nil {
		t.Fatalf("fsck failed on a healthy repository: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "refs/heads/main ok") {
		t.Errorf("fsck did not report the ref as ok:\n%s", stdout)
	}
	if !strings.Contains(stdout, "fetchable") {
		t.Errorf("fsck did not report overall fetchability:\n%s", stdout)
	}
}

// TestFsckDetectsAMissingPackBase is the case fsck exists for. A registry
// validates nothing, so a manifest can name a base that is simply not there,
// and the repository is unclonable with no other symptom.
func TestFsckDetectsAMissingPackBase(t *testing.T) {
	reg, url, _ := seeded(t)

	// Remove the base the newest packfile was cut against. Its tag is the
	// commit id, so find the one that is not the tip's ref tag or an index.
	var removed string
	for _, tag := range reg.Tags() {
		if oci.IsCommitID(tag) {
			removed = tag
			break
		}
	}
	if removed == "" {
		t.Fatal("the seeded repository has no commit-id tags to remove")
	}
	reg.DropManifest(removed)

	stdout, stderr, err := runCLI(t, "fsck", url)
	if err == nil {
		t.Fatalf("fsck reported a repository with a missing pack base as healthy\nstdout: %s", stdout)
	}
	if !strings.Contains(stderr, "packed against") {
		t.Errorf("fsck did not explain the breakage:\nstderr: %s", stderr)
	}
	if !strings.Contains(err.Error(), "not fetchable") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestFsckDetectsAPackBaseCycle: the graph is registry-supplied, so nothing
// stops it describing a cycle. fsck has to report one rather than recurse
// forever — the same failure that used to hang a fetch.
func TestFsckDetectsAPackBaseCycle(t *testing.T) {
	reg, url, tip := seeded(t)

	// Point the tip's commit manifest back at itself.
	reg.SetPackBases(t, tip, tip)
	reg.SetPackBases(t, mainTag, tip)

	stdout, stderr, err := runCLI(t, "fsck", url)
	if err == nil {
		t.Fatalf("fsck accepted a cyclic pack-base graph\nstdout: %s", stdout)
	}
	if !strings.Contains(stderr, "cycle") {
		t.Errorf("fsck did not name the cycle:\nstderr: %s", stderr)
	}
}

// TestFsckDetectsAnUnreadablePackBasesAnnotation: absent, empty or malformed is
// an error by the format, not something to treat as "no bases".
func TestFsckDetectsAnUnreadablePackBasesAnnotation(t *testing.T) {
	reg, url, _ := seeded(t)

	reg.SetPackBases(t, mainTag, "not-a-commit-id")

	stdout, _, err := runCLI(t, "fsck", url)
	if err == nil {
		t.Fatalf("fsck accepted a malformed pack-bases annotation\nstdout: %s", stdout)
	}
}

// TestFsckReportsHEAD covers the other half of the output: a repository can be
// fetchable and still advertise a HEAD nobody can check out.
func TestFsckReportsHEAD(t *testing.T) {
	_, url, _ := seeded(t)

	stdout, stderr, err := runCLI(t, "fsck", url)
	if err != nil {
		t.Fatalf("fsck: %v\nstderr: %s", err, stderr)
	}
	// SeedRepository does not record a HEAD, so fsck should say so plainly
	// rather than omit it.
	if !strings.Contains(stdout, "HEAD") {
		t.Errorf("fsck said nothing about HEAD:\n%s", stdout)
	}
}

// The _index mirror is written alongside _refs and stands in for it when _refs
// cannot be read, so a mirror left behind by a half-completed write serves an
// outdated ref list to anyone who reaches it -- and nothing else in the tool
// ever compares the two, because every normal read prefers _refs.

// TestFsckDetectsAStaleIndexMirror: the mirror still names an old commit.
func TestFsckDetectsAStaleIndexMirror(t *testing.T) {
	reg, url, _ := seeded(t)

	// Advance _refs without rewriting the mirror, which is what a push whose
	// _index write failed leaves behind.
	const moved = "fedcba9876543210fedcba9876543210fedcba98"
	if err := reg.SetIndexedRef("refs/heads/main", moved); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runCLI(t, "fsck", url)
	if err == nil {
		t.Fatalf("fsck reported a drifted mirror as healthy\nstdout: %s", stdout)
	}
	if !strings.Contains(stderr, "_index mirror") {
		t.Errorf("fsck did not name the mirror as the problem:\nstderr: %s", stderr)
	}
	if !strings.Contains(err.Error(), "drifted") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestFsckDetectsAMissingIndexMirror: absent is not the same as agreeing. A
// repository without one cannot be discovered by generic OCI tooling at all.
func TestFsckDetectsAMissingIndexMirror(t *testing.T) {
	reg, url, _ := seeded(t)
	reg.DropManifest(oci.TagOCIIndex)

	stdout, stderr, err := runCLI(t, "fsck", url)
	if err == nil {
		t.Fatalf("fsck reported a missing mirror as healthy\nstdout: %s", stdout)
	}
	if !strings.Contains(stderr, "absent") {
		t.Errorf("fsck did not say the mirror was absent:\nstderr: %s", stderr)
	}
}

// TestFsckPassesWhenTheMirrorAgrees keeps the check honest: it has to be
// capable of passing, or the two tests above prove only that it always fails.
func TestFsckPassesWhenTheMirrorAgrees(t *testing.T) {
	_, url, _ := seeded(t)

	stdout, stderr, err := runCLI(t, "fsck", url)
	if err != nil {
		t.Fatalf("fsck failed on a healthy repository: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "_index mirror matches _refs") {
		t.Errorf("fsck did not report the mirror as matching:\n%s", stdout)
	}
}
