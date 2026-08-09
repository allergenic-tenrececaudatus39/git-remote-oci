// Package gc compacts a repository stored in an OCI registry.
//
// Every push writes its own packfile artifact and its own commit-SHA tag, and
// nothing ever removes them. A repository pushed to over a long period
// therefore accumulates one packfile and one tag per push: cloning it means
// fetching every one of those packfiles and running git index-pack once per
// pack, and the registry's tag list grows without bound.
//
// Compaction rewrites each ref as a single self-contained packfile and then
// removes the intermediate commit manifests that are no longer reachable, plus
// any lock tags that have been released or have expired.
//
// The consolidation has to happen before the pruning. Fetch walks a commit's
// parents and asks the registry for each one by id, so deleting intermediate
// commit tags while the packfiles are still per-push deltas would leave a
// repository that cannot be cloned.
package gc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/mrueg/git-remote-oci/pkg/git"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// Options controls a collection run.
type Options struct {
	// DryRun reports what would change without modifying the registry.
	DryRun bool
	// Logf receives progress messages. Required.
	Logf func(format string, a ...any)
}

// Result summarises what a run did, or would do.
type Result struct {
	RefsConsolidated int
	CommitTagsPruned int
	LockTagsPruned   int
	// TagsUnprunable counts tags left behind because the registry refuses
	// manifest deletion. Compaction still succeeds; nothing is reclaimed.
	TagsUnprunable int
	TagsBefore     int
	TagsAfter      int
}

// Run compacts the repository behind client, using repo as the source of git
// objects.
//
// It must be run from a clone that already contains every commit the remote
// refs point at; the consolidated packfiles are built locally.
func Run(ctx context.Context, client *oci.Client, repo *git.Repository, opts Options) (*Result, error) {
	if opts.Logf == nil {
		return nil, fmt.Errorf("gc: Options.Logf is required")
	}

	refs, err := client.FetchRichRefIndex(ctx)
	if err != nil && !oci.IsNotFound(err) {
		return nil, fmt.Errorf("failed to read the ref index: %w", err)
	}
	if len(refs) == 0 {
		opts.Logf("nothing to do: the repository has no refs\n")
		return &Result{}, nil
	}

	before, err := client.ListAllTags(ctx)
	if err != nil {
		return nil, err
	}
	result := &Result{TagsBefore: len(before)}

	// Every commit a ref points at must be present locally, or the consolidated
	// packfile would silently omit history. Check all of them up front so the
	// run either proceeds fully or refuses, rather than half-rewriting the
	// repository.
	missing := make([]string, 0)
	for refName, entry := range refs {
		if entry.SHA == "" {
			continue
		}
		if _, err := repo.GetCommitInfo(plumbing.NewHash(entry.SHA)); err != nil {
			missing = append(missing, fmt.Sprintf("%s (%s)", refName, entry.SHA))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf(
			"these refs point at commits that are not in the local repository, so their history cannot be repacked; fetch them first:\n  %v",
			missing)
	}

	// 1. Consolidation. Each ref is rewritten as one packfile containing its
	//    whole history, so no ref depends on an intermediate commit manifest.
	refNames := make([]string, 0, len(refs))
	for refName := range refs {
		refNames = append(refNames, refName)
	}
	sort.Strings(refNames)

	keep := make(map[string]bool, len(refs))
	for _, refName := range refNames {
		entry := refs[refName]
		if entry.SHA == "" {
			continue
		}
		keep[entry.SHA] = true

		if opts.DryRun {
			opts.Logf("would repack %s (%s) into a single packfile\n", refName, short(entry.SHA))
			result.RefsConsolidated++
			continue
		}

		if err := consolidateRef(ctx, client, repo, refName, entry); err != nil {
			return nil, fmt.Errorf("failed to repack %s: %w", refName, err)
		}
		opts.Logf("repacked %s (%s)\n", refName, short(entry.SHA))
		result.RefsConsolidated++
	}

	// 2. Pruning. Only now is it safe to drop the intermediate commit
	//    manifests: every ref's history is self-contained.
	for _, tag := range before {
		switch oci.ClassifyTag(tag) {
		case oci.TagClassCommit:
			if keep[tag] {
				continue
			}
			if opts.DryRun {
				opts.Logf("would prune commit manifest %s\n", short(tag))
				result.CommitTagsPruned++
				continue
			}
			if err := client.DeleteTag(ctx, tag); err != nil {
				if errors.Is(err, oci.ErrDeletionUnsupported) {
					// Nothing to reclaim here, but the consolidation above is
					// the part that makes clones cheaper, and it has already
					// happened. Refusing the whole run would throw that away.
					result.TagsUnprunable++
					continue
				}
				return nil, fmt.Errorf("failed to prune commit manifest %s: %w", short(tag), err)
			}
			result.CommitTagsPruned++

		case oci.TagClassLock:
			reclaimable, err := client.IsLockReclaimable(ctx, tag)
			if err != nil {
				opts.Logf("leaving %s alone: %v\n", tag, err)
				continue
			}
			if !reclaimable {
				opts.Logf("leaving %s alone: it is still held\n", tag)
				continue
			}
			if opts.DryRun {
				opts.Logf("would prune released lock %s\n", tag)
				result.LockTagsPruned++
				continue
			}
			if err := client.DeleteTag(ctx, tag); err != nil {
				if errors.Is(err, oci.ErrDeletionUnsupported) {
					result.TagsUnprunable++
					continue
				}
				return nil, fmt.Errorf("failed to prune lock %s: %w", tag, err)
			}
			result.LockTagsPruned++

		case oci.TagClassMetadata, oci.TagClassRef:
			// Index tags and live ref manifests are what the repository is.
		}
	}

	if result.TagsUnprunable > 0 {
		opts.Logf("left %d tag(s) in place: this registry does not allow manifest deletion, "+
			"so the packfiles were consolidated but nothing was reclaimed\n", result.TagsUnprunable)
	}

	if opts.DryRun {
		result.TagsAfter = result.TagsBefore - result.CommitTagsPruned - result.LockTagsPruned
		return result, nil
	}

	// 3. Republish the indexes so they describe the compacted repository.
	if err := client.PushRichRefIndex(ctx, refs, nil); err != nil {
		return nil, fmt.Errorf("failed to republish the ref index: %w", err)
	}

	after, err := client.ListAllTags(ctx)
	if err != nil {
		return nil, err
	}
	result.TagsAfter = len(after)
	return result, nil
}

// consolidateRef republishes one ref as a single packfile covering its entire
// history, with no delta base exclusions.
func consolidateRef(ctx context.Context, client *oci.Client, repo *git.Repository, refName string, entry oci.RefEntry) error {
	wantHash := plumbing.NewHash(entry.SHA)
	if entry.TagObject != "" {
		// An annotated tag is published under its tag object, so pack from
		// there or the tag object itself would be left behind.
		wantHash = plumbing.NewHash(entry.TagObject)
	}

	pr, pw := io.Pipe()
	go func() {
		// No haveHashes: the point is a self-contained pack.
		_ = pw.CloseWithError(repo.CreatePackfileTo(pw, wantHash, nil))
	}()

	refTag := oci.RefManifestTag(refName)
	if refTag == "" {
		_ = pr.CloseWithError(nil)
		return fmt.Errorf("ref %q cannot be represented as an OCI tag", refName)
	}

	// No parents and no pack bases: a consolidated packfile carries the whole
	// history, so it depends on nothing. Declaring PackBasesNone is what makes
	// the pruning below safe - a fetcher of this ref will never be sent looking
	// for a commit manifest this run is about to delete.
	err := client.PushCommitStream(ctx, oci.CommitPush{
		CommitSHA: entry.SHA,
		RefName:   refName,
		RefTag:    refTag,
		// This commit is almost certainly already published; replacing its
		// packfile with a self-contained one is the point of the run.
		Rewrite: true,
	}, pr, 0)
	if err != nil {
		_ = pr.CloseWithError(err)
		return err
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
