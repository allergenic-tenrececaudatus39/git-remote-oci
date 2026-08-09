package gc_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/gc"
	"github.com/mrueg/git-remote-oci/pkg/git"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// This file covers gc.Run itself, which had no unit test at all: gc_test.go
// exercises oci.ClassifyTag, a different package. The consolidate-then-prune
// ordering is the whole design and it is destructive when wrong, so it is worth
// pinning somewhere faster and more targeted than the container end-to-end run.

// TestRunConsolidatesThenPrunes is the ordering the whole design rests on.
//
// Commit-id tags are pack bases. Pruning one before its replacement exists
// would strand every manifest naming it, so consolidation has to come first and
// the surviving ref manifest has to be self-contained afterwards.
func TestRunConsolidatesThenPrunes(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	repo, tip := registrytest.SeedRepository(t, client, 3)

	before := reg.Tags()
	if len(before) < 4 {
		t.Fatalf("expected a tag per push plus the index, got %v", before)
	}

	res, err := gc.Run(context.Background(), client, repo, gc.Options{Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("gc.Run: %v", err)
	}
	if res.RefsConsolidated != 1 {
		t.Errorf("consolidated %d refs, want 1", res.RefsConsolidated)
	}
	if res.TagsAfter >= res.TagsBefore {
		t.Errorf("gc did not reduce the tag count: %d -> %d", res.TagsBefore, res.TagsAfter)
	}

	// The surviving ref must be self-contained: nothing may remain that points
	// at a commit tag gc has removed.
	// A fresh client, so nothing is answered from the cache the push filled.
	reader := registrytest.Client(t, ts)
	manifest, err := reader.FetchManifest(context.Background(), oci.EncodeRefTag("refs/heads/main"))
	if err != nil {
		t.Fatalf("the ref manifest did not survive gc: %v", err)
	}
	bases, err := oci.ParsePackBases(manifest.Annotations)
	if err != nil {
		t.Fatalf("the consolidated manifest has unreadable pack bases: %v", err)
	}
	if len(bases) != 0 {
		t.Errorf("the consolidated pack still declares bases %v; it must stand alone", bases)
	}
	if rev := manifest.Annotations[ocispec.AnnotationRevision]; rev != tip {
		t.Errorf("ref manifest points at %s, want the tip %s", rev, tip)
	}
}

// TestRunDryRunTouchesNothing: a dry run reports and writes nothing.
func TestRunDryRunTouchesNothing(t *testing.T) {
	reg := registrytest.New()
	client := registrytest.Client(t, reg.Serve(t))
	repo, _ := registrytest.SeedRepository(t, client, 3)

	before := reg.Tags()

	res, err := gc.Run(context.Background(), client, repo, gc.Options{
		DryRun: true,
		Logf:   func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("gc.Run: %v", err)
	}
	if res.RefsConsolidated == 0 {
		t.Error("a dry run should still report what it would consolidate")
	}

	after := reg.Tags()
	if len(after) != len(before) {
		t.Errorf("a dry run changed the tag list: %v -> %v", before, after)
	}
	if deleted := len(reg.Deletions()); deleted != 0 {
		t.Errorf("a dry run issued %d deletions", deleted)
	}
}

// TestRunOnRegistryRefusingDeletion: consolidation still has to happen where
// manifests cannot be removed. The space is not reclaimed, but nothing is lost
// and the repository stays clonable.
func TestRunOnRegistryRefusingDeletion(t *testing.T) {
	reg := registrytest.New()
	reg.RefuseDelete = true
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	repo, _ := registrytest.SeedRepository(t, client, 3)

	res, err := gc.Run(context.Background(), client, repo, gc.Options{Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("gc must not fail on a registry that refuses deletion: %v", err)
	}
	if res.RefsConsolidated != 1 {
		t.Errorf("consolidated %d refs, want 1", res.RefsConsolidated)
	}
	if res.TagsUnprunable == 0 {
		t.Error("tags that could not be pruned should be counted and reported")
	}

	reader := registrytest.Client(t, ts)
	manifest, err := reader.FetchManifest(context.Background(), oci.EncodeRefTag("refs/heads/main"))
	if err != nil {
		t.Fatalf("the ref manifest did not survive: %v", err)
	}
	if bases, _ := oci.ParsePackBases(manifest.Annotations); len(bases) != 0 {
		t.Errorf("the pack was not consolidated: bases %v", bases)
	}
}

// TestRunOnEmptyRepositoryIsANoOp guards the degenerate case.
func TestRunOnEmptyRepositoryIsANoOp(t *testing.T) {
	reg := registrytest.New()
	client := registrytest.Client(t, reg.Serve(t))

	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "-b", "main", ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	t.Setenv("GIT_DIR", filepath.Join(dir, ".git"))
	repo, err := git.OpenRepository()
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	res, err := gc.Run(context.Background(), client, repo, gc.Options{Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("gc.Run on an empty repository: %v", err)
	}
	if res.RefsConsolidated != 0 {
		t.Errorf("consolidated %d refs on an empty repository", res.RefsConsolidated)
	}
}

// TestRunRequiresALogger pins the contract rather than letting a nil call panic
// deep inside the run.
func TestRunRequiresALogger(t *testing.T) {
	reg := registrytest.New()
	client := registrytest.Client(t, reg.Serve(t))
	if _, err := gc.Run(context.Background(), client, nil, gc.Options{}); err == nil {
		t.Error("gc.Run without a logger should be refused")
	}
}
