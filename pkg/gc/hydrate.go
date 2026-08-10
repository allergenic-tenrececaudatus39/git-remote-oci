package gc

import (
	"context"
	"fmt"
	"os"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/pkg/git"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// Compaction from the registry alone.
//
// Consolidation builds each ref's replacement packfile out of git objects, and
// those objects used to have to be sitting in a local clone: gc refused unless
// every ref tip was present. That made the one job most worth automating the
// one job that could not be — compaction is a maintenance task that wants to run
// on a schedule next to the registry, and instead it needed a machine holding a
// full clone of every repository it was to look after.
//
// The objects are in the registry, which is the whole point of the registry. So
// when they are not local, they are fetched into a scratch repository and the
// consolidated packs are built from that. The scratch store is thrown away
// afterwards; nothing is written to any repository the user has.

// hydrated is a scratch repository holding objects pulled from the registry.
type hydrated struct {
	repo *git.Repository
	dir  string
}

// Close removes the scratch store.
func (h *hydrated) Close() {
	if h == nil || h.dir == "" {
		return
	}
	_ = os.RemoveAll(h.dir)
}

// hydrate builds a scratch repository containing every object the named refs
// need, by fetching their packfiles from the registry.
//
// It needs room for as much history as it fetches. That goes in the system
// temporary directory, so $TMPDIR is how to put it somewhere with space when
// the default is a small tmpfs — the same consideration the push path has, for
// the same reason.
func hydrate(ctx context.Context, client *oci.Client, entries []oci.RefEntry, logf func(string, ...any)) (*hydrated, error) {
	dir, err := os.MkdirTemp("", "git-remote-oci-gc-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create a scratch object store: %w", err)
	}
	h := &hydrated{dir: dir}

	if _, err := gogit.PlainInit(dir, true); err != nil {
		h.Close()
		return nil, fmt.Errorf("failed to initialise the scratch object store: %w", err)
	}
	// Only for the subprocess helpers, which is all the import path uses. The
	// store is re-opened once everything is in it, because go-git caches the
	// pack list it found when it opened and would not see the imports.
	writer, err := git.OpenRepositoryAt(dir)
	if err != nil {
		h.Close()
		return nil, err
	}

	order, err := manifestOrder(ctx, client, entries)
	if err != nil {
		h.Close()
		return nil, err
	}

	logf("fetching %d packfile(s) from the registry to repack from\n", len(order))
	for _, manifest := range order {
		stream, streamErr := client.FetchPackfileStream(ctx, manifest.manifest)
		if streamErr != nil {
			h.Close()
			return nil, fmt.Errorf("failed to fetch the packfile for %s: %w", short(manifest.sha), streamErr)
		}
		_, importErr := writer.ImportPackfile(stream)
		_ = stream.Close()
		if importErr != nil {
			h.Close()
			return nil, fmt.Errorf("failed to import the packfile for %s: %w", short(manifest.sha), importErr)
		}
	}

	reader, err := git.OpenRepositoryAt(dir)
	if err != nil {
		h.Close()
		return nil, err
	}
	h.repo = reader
	return h, nil
}

// stagedManifest is a commit manifest and the commit it belongs to.
type stagedManifest struct {
	sha      string
	manifest *ocispec.Manifest
}

// manifestOrder resolves the pack-base graph for the given tips and returns the
// manifests in an order safe to import: a packfile is thin, and `index-pack
// --fix-thin` can only complete it once the objects it deltas against are
// already in the store, so bases come before the packs cut against them.
func manifestOrder(ctx context.Context, client *oci.Client, entries []oci.RefEntry) ([]stagedManifest, error) {
	chain, _ := client.FetchPackChain(ctx)

	seen := map[string]bool{}
	var levels [][]string
	var frontier []string
	for _, entry := range entries {
		sha := entry.SHA
		if entry.TagObject != "" {
			sha = entry.TagObject
		}
		if sha == "" || seen[sha] {
			continue
		}
		seen[sha] = true
		frontier = append(frontier, sha)
	}

	resolved := map[string]*ocispec.Manifest{}
	for len(frontier) > 0 {
		levels = append(levels, frontier)
		var next []string
		for _, sha := range frontier {
			manifest, err := client.FetchManifest(ctx, sha)
			if err != nil {
				return nil, fmt.Errorf("failed to fetch the manifest for %s: %w", short(sha), err)
			}
			resolved[sha] = manifest

			bases, err := oci.ParsePackBases(manifest.Annotations)
			if err != nil {
				return nil, fmt.Errorf("commit %s: %w", short(sha), err)
			}
			// The published chain (§6.1) is only a shortcut for discovering the
			// graph in fewer round trips; the annotation above is what decides
			// what has to be imported, so anything the chain adds beyond it is
			// extra history rather than a correction.
			for _, base := range append(bases, chain[sha]...) {
				if seen[base] {
					continue
				}
				seen[base] = true
				next = append(next, base)
			}
		}
		frontier = next
	}

	// Deepest level first: those packs depend on nothing still to come.
	var order []stagedManifest
	for i := len(levels) - 1; i >= 0; i-- {
		for _, sha := range levels[i] {
			order = append(order, stagedManifest{sha: sha, manifest: resolved[sha]})
		}
	}
	return order, nil
}

// missingLocally reports which of the given refs the repository cannot repack
// from its own objects.
func missingLocally(repo *git.Repository, refs map[string]oci.RefEntry) []string {
	if repo == nil {
		names := make([]string, 0, len(refs))
		for name, entry := range refs {
			if entry.SHA != "" {
				names = append(names, name)
			}
		}
		return names
	}

	var missing []string
	for name, entry := range refs {
		if entry.SHA == "" {
			continue
		}
		if _, err := repo.GetCommitInfo(plumbing.NewHash(entry.SHA)); err != nil {
			missing = append(missing, name)
		}
	}
	return missing
}
