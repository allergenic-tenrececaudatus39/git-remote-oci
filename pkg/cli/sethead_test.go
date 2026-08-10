package cli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// A repository's default branch is adopted from the first branch pushed to it
// and, until this command, could never move. These cover the thing that makes
// that safe to change: it is the recorded HEAD that a clone reads, and it must
// never end up naming something the repository does not publish.

// seedTwoBranches publishes refs/heads/main and refs/heads/other, with HEAD
// adopted from main because it was pushed first.
func seedTwoBranches(t *testing.T) (string, *oci.Client) {
	t.Helper()

	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	_, tip := registrytest.SeedRepository(t, client, 2)

	refs, err := client.FetchRichRefIndex(context.Background())
	if err != nil {
		t.Fatalf("FetchRichRefIndex: %v", err)
	}
	refs["refs/heads/other"] = oci.RefEntry{SHA: tip}
	// Record main as the default, which is what a real push does through
	// headHintFor. The seeding helper does not, and without it there would be
	// no "before" for the command to move away from.
	if err := client.PushRichRefIndexWithHead(context.Background(), refs, nil, "refs/heads/main"); err != nil {
		t.Fatalf("publishing a second branch: %v", err)
	}
	return registrytest.URL(ts), client
}

func TestSetHeadReportsTheCurrentDefault(t *testing.T) {
	url, _ := seedTwoBranches(t)

	stdout, stderr, err := runCLI(t, "set-head", url)
	if err != nil {
		t.Fatalf("set-head with no ref failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "refs/heads/main") {
		t.Errorf("set-head did not report the recorded default:\n%s", stdout)
	}
}

// The point of the command: the first branch pushed is not a life sentence.
func TestSetHeadMovesTheDefault(t *testing.T) {
	url, client := seedTwoBranches(t)

	stdout, stderr, err := runCLI(t, "set-head", url, "refs/heads/other")
	if err != nil {
		t.Fatalf("set-head failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "refs/heads/main") || !strings.Contains(stdout, "refs/heads/other") {
		t.Errorf("set-head did not report the move:\n%s", stdout)
	}

	// What a clone actually reads.
	head, err := client.FetchHead(context.Background())
	if err != nil {
		t.Fatalf("FetchHead: %v", err)
	}
	if head != "refs/heads/other" {
		t.Errorf("recorded HEAD is %q, want refs/heads/other", head)
	}

	// And the refs are still all there: this republishes the index, so losing
	// one would be the obvious way to get it wrong.
	refs, err := client.FetchRichRefIndex(context.Background())
	if err != nil {
		t.Fatalf("FetchRichRefIndex: %v", err)
	}
	for _, want := range []string{"refs/heads/main", "refs/heads/other"} {
		if _, ok := refs[want]; !ok {
			t.Errorf("%s disappeared from the index", want)
		}
	}
}

// A short name is what anyone will type.
func TestSetHeadAcceptsAShortBranchName(t *testing.T) {
	url, client := seedTwoBranches(t)

	if _, stderr, err := runCLI(t, "set-head", url, "other"); err != nil {
		t.Fatalf("set-head with a short name failed: %v\nstderr: %s", err, stderr)
	}
	if head, _ := client.FetchHead(context.Background()); head != "refs/heads/other" {
		t.Errorf("recorded HEAD is %q, want refs/heads/other", head)
	}
}

// A typo must not become the default. Recording a HEAD that names nothing is
// the state the deletion path already goes out of its way to avoid.
func TestSetHeadRefusesABranchThatIsNotThere(t *testing.T) {
	url, client := seedTwoBranches(t)

	_, stderr, err := runCLI(t, "set-head", url, "trunk")
	if err == nil {
		t.Fatal("set-head accepted a branch the repository does not publish")
	}
	// The useful half of the error is what could have been typed instead.
	if !strings.Contains(err.Error()+stderr, "main") {
		t.Errorf("the error does not say what is available: %v", err)
	}
	if head, _ := client.FetchHead(context.Background()); head != "refs/heads/main" {
		t.Errorf("a rejected set-head still moved HEAD to %q", head)
	}
}

// HEAD is a symbolic ref to a branch. Pointing it at a tag gives a clone a
// detached head and nothing to commit on.
func TestSetHeadRefusesATag(t *testing.T) {
	url, client := seedTwoBranches(t)

	refs, err := client.FetchRichRefIndex(context.Background())
	if err != nil {
		t.Fatalf("FetchRichRefIndex: %v", err)
	}
	refs["refs/tags/v1"] = oci.RefEntry{SHA: refs["refs/heads/main"].SHA}
	if err := client.PushRichRefIndex(context.Background(), refs, nil); err != nil {
		t.Fatalf("publishing a tag: %v", err)
	}

	if _, _, err := runCLI(t, "set-head", url, "refs/tags/v1"); err == nil {
		t.Fatal("set-head accepted a tag")
	}
	if head, _ := client.FetchHead(context.Background()); head != "refs/heads/main" {
		t.Errorf("a rejected set-head still moved HEAD to %q", head)
	}
}
