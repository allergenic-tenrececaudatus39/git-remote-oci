package git_test

import (
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/mrueg/git-remote-oci/pkg/git"
)

// GitDir is now the only place the git directory is worked out. Five callers
// used to compute it inline and one of them had already got it wrong, so what
// it answers — including what it answers when it cannot tell — is worth
// pinning rather than leaving to whichever caller notices first.

func TestGitDirPrefersGitDirEnv(t *testing.T) {
	dir := t.TempDir()
	if _, err := gogit.PlainInit(dir, false); err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	gitDir := filepath.Join(dir, ".git")
	t.Setenv("GIT_DIR", gitDir)

	got, ok := git.GitDir()
	if !ok {
		t.Errorf("ok = false for a valid GIT_DIR")
	}
	if got != gitDir {
		t.Errorf("GitDir() = %q, want %q", got, gitDir)
	}
}

// A bare repository is the shape every clone target has, and the git directory
// is the repository itself — not a ".git" beneath it. Appending one is the bug
// that used to write the shallow boundary into a directory that did not exist.
func TestGitDirDoesNotAppendDotGitToABareRepository(t *testing.T) {
	dir := t.TempDir()
	if _, err := gogit.PlainInit(dir, true); err != nil {
		t.Fatalf("PlainInit bare: %v", err)
	}
	t.Setenv("GIT_DIR", dir)

	got, ok := git.GitDir()
	if !ok {
		t.Errorf("ok = false for a valid bare GIT_DIR")
	}
	if got != dir {
		t.Errorf("GitDir() = %q, want the bare repository itself (%q)", got, dir)
	}
}

// Without GIT_DIR the directory is found by walking upwards, which is the case
// a caller reaching for a literal ".git" gets wrong from a subdirectory.
func TestGitDirWalksUpFromASubdirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := gogit.PlainInit(dir, false); err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	nested := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", "")
	t.Chdir(nested)

	got, ok := git.GitDir()
	if !ok {
		t.Errorf("ok = false when walking up should have found the repository")
	}
	// t.TempDir can hand back a symlinked path (/tmp -> /private/tmp and the
	// like), so compare what the filesystem resolves rather than the strings.
	want := filepath.Join(dir, ".git")
	gotEval, _ := filepath.EvalSymlinks(got)
	wantEval, _ := filepath.EvalSymlinks(want)
	if gotEval != wantEval {
		t.Errorf("GitDir() = %q, want %q", got, want)
	}
}

// The fallback is what the five inlined copies used to do, and callers that
// must produce a path rely on getting one. ok says it was a guess.
func TestGitDirGuessesWhenNothingIsFound(t *testing.T) {
	t.Setenv("GIT_DIR", "")
	t.Chdir(t.TempDir())

	got, ok := git.GitDir()
	if ok {
		t.Errorf("ok = true outside any repository")
	}
	if got == "" {
		t.Fatal("no path returned; callers that cannot check ok would build paths from an empty string")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("GitDir() = %q, want an absolute path", got)
	}
	if filepath.Base(got) != ".git" {
		t.Errorf("GitDir() = %q, want the historical .git guess", got)
	}
}

// GIT_DIR pointing somewhere that is not a git directory still wins over the
// guess: git handed it over, and declining it would silently address a
// different repository.
func TestGitDirKeepsAnUnrecognisedGitDirEnv(t *testing.T) {
	target := t.TempDir()
	t.Setenv("GIT_DIR", target)
	t.Chdir(t.TempDir())

	got, ok := git.GitDir()
	if ok {
		t.Errorf("ok = true for a GIT_DIR that is not a git directory")
	}
	if got != target {
		t.Errorf("GitDir() = %q, want GIT_DIR verbatim (%q)", got, target)
	}
}

func TestObjectsDirHonoursGitObjectDirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := gogit.PlainInit(dir, false); err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	t.Setenv("GIT_DIR", filepath.Join(dir, ".git"))

	if got, ok := git.ObjectsDir(); !ok || got != filepath.Join(dir, ".git", "objects") {
		t.Errorf("ObjectsDir() = %q (ok=%v), want the repository's objects directory", got, ok)
	}

	// git may hand the process an object directory of its own; that is the one
	// to use, not the one under GIT_DIR.
	elsewhere := t.TempDir()
	t.Setenv("GIT_OBJECT_DIRECTORY", elsewhere)
	if got, ok := git.ObjectsDir(); !ok || got != elsewhere {
		t.Errorf("ObjectsDir() = %q (ok=%v), want GIT_OBJECT_DIRECTORY (%q)", got, ok, elsewhere)
	}
}
