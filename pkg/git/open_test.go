package git_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mrueg/git-remote-oci/pkg/git"
)

// OpenRepository now resolves the git directory itself so it can build the
// storage with a large-object threshold, instead of letting PlainOpen construct
// storage internally. These tests cover the discovery paths that rewrite
// touched, because failing to open a repository at all would be far worse than
// opening it without the threshold.

func initRepo(t *testing.T, dir string, bare bool) {
	t.Helper()
	args := []string{"init", "-q"}
	if bare {
		args = append(args, "--bare")
	}
	args = append(args, dir)
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
}

func TestOpenRepositoryWithGitDirPointingAtDotGit(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir, false)
	t.Setenv("GIT_DIR", filepath.Join(dir, ".git"))

	if _, err := git.OpenRepository(); err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}
}

func TestOpenRepositoryWithGitDirPointingAtABareRepo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bare.git")
	initRepo(t, dir, true)
	t.Setenv("GIT_DIR", dir)

	if _, err := git.OpenRepository(); err != nil {
		t.Fatalf("OpenRepository on a bare repository: %v", err)
	}
}

func TestOpenRepositoryDiscoversFromWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir, false)
	// A nested directory must still find the repository by walking up.
	nested := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	t.Setenv("GIT_DIR", "")
	os.Unsetenv("GIT_DIR")
	restore := chdir(t, nested)
	defer restore()

	if _, err := git.OpenRepository(); err != nil {
		t.Fatalf("OpenRepository from a nested directory: %v", err)
	}
}

func TestOpenRepositoryFailsOutsideAnyRepository(t *testing.T) {
	dir := t.TempDir()
	os.Unsetenv("GIT_DIR")
	restore := chdir(t, dir)
	defer restore()

	if _, err := git.OpenRepository(); err == nil {
		t.Error("OpenRepository succeeded outside a repository")
	}
}

// TestOpenRepositoryWithBogusGitDirFallsBack: a GIT_DIR that is not a git
// directory must not be accepted by the fast path, and the fallback must then
// produce a clear failure rather than a confusing one.
func TestOpenRepositoryWithBogusGitDirFallsBack(t *testing.T) {
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "nope"))
	if _, err := git.OpenRepository(); err == nil {
		t.Error("OpenRepository accepted a GIT_DIR that is not a git directory")
	}
}

// chdir changes directory for the duration of a test. t.Chdir exists in newer
// Go, but doing it explicitly keeps the restore visible.
func chdir(t *testing.T, dir string) func() {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	return func() { _ = os.Chdir(prev) }
}
