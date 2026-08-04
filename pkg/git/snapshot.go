package git

import (
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
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
// It shells out to git because the object set is `git rev-list --objects
// --max-count=1`, which go-git has no direct equivalent for; a repository
// without git on PATH simply publishes no snapshot.
func (r *Repository) CreateSnapshotPackfileTo(writer io.Writer, tip plumbing.Hash) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("%w: git is not on PATH", ErrSnapshotUnavailable)
	}

	gitDir, workDir := gitDirArg()

	peeled := tip
	if tagObj, err := r.repo.TagObject(tip); err == nil {
		peeled = tagObj.Target
	}

	// --max-count=1 stops after the tip commit, and --objects then lists every
	// tree and blob reachable from it. Ancestors are never walked, which is
	// exactly the difference between this and the incremental packfile.
	revList := exec.Command("git", "--git-dir="+gitDir, "rev-list",
		"--objects", "--max-count=1", peeled.String())
	revList.Dir = workDir
	revList.Stderr = io.Discard
	objects, err := revList.Output()
	if err != nil {
		return fmt.Errorf("%w: rev-list failed: %w", ErrSnapshotUnavailable, err)
	}

	// rev-list prints "<oid> [path]"; pack-objects wants just the oid.
	var ids strings.Builder
	for _, line := range strings.Split(string(objects), "\n") {
		if line == "" {
			continue
		}
		oid, _, _ := strings.Cut(line, " ")
		ids.WriteString(oid)
		ids.WriteByte('\n')
	}
	if ids.Len() == 0 {
		return fmt.Errorf("%w: %s reaches no objects", ErrSnapshotUnavailable, tip)
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
