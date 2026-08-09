package cli_test

import (
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/pkg/cli"
)

// TestEveryReservedNameDispatchesToItsSubcommand is the other half of
// TestCollidingRemoteNameIsRefused: without GIT_DIR the same argv must reach
// the subcommand rather than opening a protocol session on stdin.
//
// A helper session driven by the "quit" stdin these tests supply returns no
// error and writes nothing, so that pair is the signature to rule out.
func TestEveryReservedNameDispatchesToItsSubcommand(t *testing.T) {
	for _, name := range cli.ReservedNames() {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, err := runCLI(t, name, "oci://localhost:1/does-not-exist")
			if err == nil && stdout == "" {
				t.Fatalf("%q started a protocol session instead of dispatching", name)
			}
			if strings.Contains(stderr, "unrecognised arguments") {
				t.Errorf("%q was not recognised as a subcommand: %s", name, stderr)
			}
		})
	}
}

// TestTopLevelUsageMatchesSubcommandUsage stops the two usage texts drifting.
//
// The subcommand table carries a short argument summary for the top-level help,
// and each subcommand repeats it in its own -h output. They were written by
// hand and three of the six disagreed: break-lock, lfs-lock and lfs-unlock all
// take flags that the summary did not mention, and lfs-unlock accepts a lock id
// as well as a path.
func TestTopLevelUsageMatchesSubcommandUsage(t *testing.T) {
	for _, name := range cli.ReservedNames() {
		summary, ok := cli.UsageSummary(name)
		if !ok || summary == "" {
			continue // version and help take no arguments and have no flag set
		}
		t.Run(name, func(t *testing.T) {
			// -h makes the flag set print its own usage and stop.
			_, stderr, _ := runCLI(t, name, "-h")
			want := "usage: git-remote-oci " + name + " " + summary
			if !strings.Contains(stderr, want) {
				t.Errorf("top-level help advertises\n  %s\nbut %s -h says\n  %s",
					want, name, firstUsageLine(stderr))
			}
		})
	}
}

func firstUsageLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "usage:") {
			return line
		}
	}
	return "(no usage line)"
}

func TestBreakLockRequiresURLAndRef(t *testing.T) {
	for _, args := range [][]string{
		{"break-lock"},
		{"break-lock", "oci://example.test/repo"},
		{"break-lock", "oci://example.test/repo", "refs/heads/main", "extra"},
	} {
		_, _, err := runCLI(t, args...)
		if err == nil {
			t.Errorf("break-lock %v should have been rejected", args[1:])
		}
	}
}

// TestBreakLockIsNotNamedUnlock pins the rename. "unlock" read as a sibling of
// "lfs-unlock", but lfs-unlock releases a lock you hold while this one takes a
// lock away from whoever holds it.
func TestBreakLockIsNotNamedUnlock(t *testing.T) {
	for _, name := range cli.ReservedNames() {
		if name == "unlock" {
			t.Error(`"unlock" is reserved again; it is too easily read as the counterpart of lfs-unlock`)
		}
	}
}

func TestFsckRequiresExactlyOneURL(t *testing.T) {
	for _, args := range [][]string{
		{"fsck"},
		{"fsck", "a", "b"},
	} {
		_, _, err := runCLI(t, args...)
		if err == nil {
			t.Errorf("fsck %v should have been rejected", args[1:])
		}
	}
}
