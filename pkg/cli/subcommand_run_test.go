package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The dispatch tests cover which subcommand runs. These cover what the
// subcommands then do: until now nothing invoked fsck, break-lock or the lfs-*
// bodies at all, only their argument checks.

// emptyRegistry answers like a registry hosting a repository that exists but
// holds nothing: no tags, no manifests.
func emptyRegistry(t *testing.T) *httptest.Server {
	t.Helper()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "repo", "tags": []string{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// registryURL renders a test server as the oci:// URL a user would type.
func registryURL(ts *httptest.Server) string {
	return "oci://" + strings.TrimPrefix(ts.URL, "http://") + "/repo"
}

func TestFsckOnAnEmptyRepository(t *testing.T) {
	ts := emptyRegistry(t)

	stdout, stderr, err := runCLI(t, "fsck", registryURL(ts))
	if err != nil {
		t.Fatalf("fsck on an empty repository should not fail: %v\nstderr: %s", err, stderr)
	}
	if stdout == "" {
		t.Error("fsck reported nothing at all")
	}
}

// TestBreakLockOnAnUnlockedRefIsANoOp: reporting "not locked" is the correct
// answer, not an error. A user reaching for break-lock is already having a bad
// day and should not be told off for guessing wrong about which ref is stuck.
func TestBreakLockOnAnUnlockedRefIsANoOp(t *testing.T) {
	ts := emptyRegistry(t)

	stdout, stderr, err := runCLI(t, "break-lock", registryURL(ts), "refs/heads/main")
	if err != nil {
		t.Fatalf("break-lock on an unlocked ref: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "not locked") {
		t.Errorf("expected it to say the ref is not locked, got: %q", stdout)
	}
}

func TestLFSLocksOnAnEmptyRepository(t *testing.T) {
	ts := emptyRegistry(t)

	stdout, stderr, err := runCLI(t, "lfs-locks", registryURL(ts))
	if err != nil {
		t.Fatalf("lfs-locks on an empty repository: %v\nstderr: %s", err, stderr)
	}
	if stdout == "" {
		t.Error("lfs-locks reported nothing at all")
	}
}

// TestSubcommandsReportAnUnreachableRegistry: every subcommand takes a URL, and
// a registry that is not there has to produce a legible failure rather than a
// panic or a success.
func TestSubcommandsReportAnUnreachableRegistry(t *testing.T) {
	const dead = "oci://127.0.0.1:1/repo"

	for _, args := range [][]string{
		{"fsck", dead},
		{"break-lock", dead, "refs/heads/main"},
		{"lfs-locks", dead},
		{"lfs-lock", dead, "art/hero.psd"},
		{"lfs-unlock", dead, "art/hero.psd"},
	} {
		t.Run(args[0], func(t *testing.T) {
			_, _, err := runCLI(t, args...)
			if err == nil {
				t.Fatalf("%v against an unreachable registry reported success", args)
			}
			if strings.Contains(err.Error(), "panic") {
				t.Errorf("unhelpful failure: %v", err)
			}
		})
	}
}

// TestGCOutsideAGitRepositoryExplainsItself: gc builds its packfiles from local
// objects, so it needs a repository — and should say so rather than failing
// somewhere deep in go-git.
func TestGCOutsideAGitRepositoryExplainsItself(t *testing.T) {
	ts := emptyRegistry(t)
	t.Chdir(t.TempDir())

	// An empty repository short-circuits before the local objects are needed,
	// so this asserts only that it does not fail confusingly.
	if _, _, err := runCLI(t, "gc", registryURL(ts)); err != nil {
		if !strings.Contains(err.Error(), "git repository") {
			t.Errorf("gc outside a repository should explain itself, got: %v", err)
		}
	}
}
