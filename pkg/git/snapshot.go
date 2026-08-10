package git

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// ErrSnapshotUnavailable reports that a tip snapshot could not be produced.
//
// It is not fatal to a push: the snapshot is an optimisation for shallow
// clones, and a repository without one still fetches correctly, just at the
// cost of the whole history.
var ErrSnapshotUnavailable = fmt.Errorf("cannot build a tip snapshot")

// CreateSnapshotPackfileTo writes a self-contained packfile holding exactly the
// objects reachable from tip's tree, and the tip commit itself.
//
// This is the depth-1 object set: enough to check the commit out, with none of
// its ancestry. It is deliberately *not* thin — a shallow clone has nothing to
// delta against, so a pack with external bases would be unusable.
//
// Still shells out for the packing itself, which go-git cannot do thinly or as
// compactly; a repository without git on PATH simply publishes no snapshot.
func (r *Repository) CreateSnapshotPackfileTo(writer io.Writer, tip plumbing.Hash) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("%w: git is not on PATH", ErrSnapshotUnavailable)
	}

	gitDir, workDir := gitDirArg()

	peeled := tip
	if tagObj, err := r.repo.TagObject(tip); err == nil {
		peeled = tagObj.Target
	}

	// The commit, its tree, and everything under that tree — and nothing else.
	// Not walking the parents is exactly the difference between this and the
	// incremental packfile.
	objects, err := r.snapshotObjects(peeled)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSnapshotUnavailable, err)
	}
	if len(objects) == 0 {
		return fmt.Errorf("%w: %s reaches no objects", ErrSnapshotUnavailable, tip)
	}

	var ids strings.Builder
	for _, oid := range objects {
		ids.WriteString(oid.String())
		ids.WriteByte('\n')
	}

	// No --thin: the result must stand on its own.
	pack := exec.Command("git", "--git-dir="+gitDir, "pack-objects",
		"--stdout", "--delta-base-offset", "--quiet")
	pack.Dir = workDir
	pack.Stdin = strings.NewReader(ids.String())
	pack.Stderr = io.Discard
	pack.Stdout = writer

	if err := pack.Run(); err != nil {
		return fmt.Errorf("%w: pack-objects failed: %w", ErrSnapshotUnavailable, err)
	}
	return nil
}

// snapshotObjects lists the commit, its tree, and every object beneath it.
//
// This is what `rev-list --objects --max-count=1` produced. Walking it here
// rather than parsing that output means the ancestry is not merely truncated
// after the fact but never visited, and there is no second process whose stdout
// has to be reparsed into the ids the first one already knew.
//
// A tree entry that cannot be read stops the walk with an error rather than
// being skipped: a snapshot is defined as self-contained, and one quietly
// missing an object would produce a shallow clone that fails on checkout.
func (r *Repository) snapshotObjects(commitHash plumbing.Hash) ([]plumbing.Hash, error) {
	commit, err := object.GetCommit(r.storer, commitHash)
	if err != nil {
		return nil, fmt.Errorf("failed to read commit %s: %w", commitHash, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to read the tree of %s: %w", commitHash, err)
	}

	objects := []plumbing.Hash{commitHash, tree.Hash}
	seen := map[plumbing.Hash]bool{commitHash: true, tree.Hash: true}

	walker := object.NewTreeWalker(tree, true, seen)
	defer walker.Close()
	for {
		_, entry, err := walker.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to walk the tree of %s: %w", commitHash, err)
		}
		if seen[entry.Hash] {
			continue
		}
		seen[entry.Hash] = true
		objects = append(objects, entry.Hash)
	}
	return objects, nil
}
