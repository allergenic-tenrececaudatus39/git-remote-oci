package git_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/mrueg/git-remote-oci/pkg/git"
)

// TestShallowBoundaryCountsGenerations pins the depth semantics.
//
// This used to be derived from `git rev-list --max-count=N`, which counts
// *commits*. Depth counts generations, and the two stop agreeing the moment the
// history has a merge in it: a merge's two parents sit at the same generation
// but consume two of the count, so the boundary landed short of the depth that
// was asked for and the clone was truncated above where git said it would be.
//
// The history here is a diamond — a merge whose two sides both descend from one
// root:
//
//	    merge        generation 0
//	   /     \
//	side    main     generation 1
//	   \     /
//	    root         generation 2
func TestShallowBoundaryCountsGenerations(t *testing.T) {
	dir := t.TempDir()
	g, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := g.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	t.Setenv("GIT_DIR", filepath.Join(dir, ".git"))

	commit := func(name, content, message string, parents ...plumbing.Hash) plumbing.Hash {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatal(err)
		}
		h, err := wt.Commit(message, &gogit.CommitOptions{
			Author:  &object.Signature{Name: "T", Email: "t@example.com", When: time.Now()},
			Parents: parents,
		})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}

	root := commit("a.txt", "root\n", "root")
	side := commit("b.txt", "side\n", "side", root)
	main := commit("c.txt", "main\n", "main", root)
	merge := commit("d.txt", "merge\n", "merge", main, side)

	repo, err := git.OpenRepository()
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	for _, tc := range []struct {
		name  string
		depth int
		want  []plumbing.Hash
	}{
		// Depth 1 is the merge alone; both its parents are outside.
		{"depth 1 stops at the merge", 1, []plumbing.Hash{merge}},
		// Depth 2 takes both sides of the diamond. Counting commits would have
		// stopped after the merge and one side, leaving the other side's parent
		// unaccounted for and the boundary in the wrong place.
		{"depth 2 takes both sides of the merge", 2, []plumbing.Hash{main, side}},
		// Depth 3 reaches the root, which has no parents, so nothing is cut.
		{"depth 3 reaches the root and needs no boundary", 3, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.ShallowBoundary(merge.String(), tc.depth)
			if err != nil {
				t.Fatalf("ShallowBoundary: %v", err)
			}
			want := make(map[string]bool, len(tc.want))
			for _, h := range tc.want {
				want[h.String()] = true
			}
			if len(got) != len(want) {
				t.Fatalf("boundary = %v, want %d commits (%v)", got, len(want), tc.want)
			}
			for _, sha := range got {
				if !want[sha] {
					t.Errorf("boundary contains %s, which is not one of %v", sha, tc.want)
				}
			}
		})
	}
}
