package cli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/pkg/cli"
)

// runCLIEnv invokes the dispatcher with an explicit environment.
//
// The environment is explicit rather than inherited because the dispatcher
// keys off GIT_DIR, and `go test` is routinely run from inside a repository
// by a shell that may or may not export it. Reading the real environment
// would make these tests pass or fail according to how they were started.
func runCLIEnv(t *testing.T, environ map[string]string, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	var out, errBuf strings.Builder
	err = cli.Run(context.Background(), cli.Env{
		Args:    args,
		Version: "test-version",
		Stdin:   strings.NewReader("quit\n"),
		Stdout:  &out,
		Stderr:  &errBuf,
		Getenv:  func(k string) string { return environ[k] },
	})
	return out.String(), errBuf.String(), err
}

// runCLI invokes the dispatcher as if from a shell: no GIT_DIR, so nothing is
// mistaken for git calling the helper.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return runCLIEnv(t, nil, args...)
}

func TestVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		t.Run(arg, func(t *testing.T) {
			stdout, _, err := runCLI(t, arg)
			if err != nil {
				t.Fatalf("Run(%q): %v", arg, err)
			}
			if !strings.Contains(stdout, "test-version") {
				t.Errorf("version output %q does not contain the version", stdout)
			}
		})
	}
}

func TestHelp(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			stdout, _, err := runCLI(t, arg)
			if err != nil {
				t.Fatalf("Run(%q): %v", arg, err)
			}
			if !strings.Contains(stdout, "gc") || !strings.Contains(stdout, "oci://") {
				t.Errorf("help output looks wrong:\n%s", stdout)
			}
		})
	}
}

// TestUsageListsEverySubcommand pins the single-declaration property: the help
// text and the reserved-name set are both derived from the subcommand table, so
// adding a subcommand without reserving its name is not expressible.
func TestUsageListsEverySubcommand(t *testing.T) {
	stdout, _, err := runCLI(t, "help")
	if err != nil {
		t.Fatalf("help: %v", err)
	}
	names := cli.ReservedNames()
	if len(names) == 0 {
		t.Fatal("ReservedNames is empty")
	}
	for _, name := range names {
		if !strings.Contains(stdout, name) {
			t.Errorf("subcommand %q is reserved but absent from the usage text", name)
		}
	}
}

// TestRemoteHelperInvocationStillWins is the important one: git always invokes
// the helper as `git-remote-oci <remote> <url>`, and adding subcommands must
// not break that. Two arguments whose first is not reserved go to the helper.
func TestRemoteHelperInvocationStillWins(t *testing.T) {
	// "quit" as the only input makes the helper exit immediately and cleanly,
	// without needing a registry.
	stdout, _, err := runCLIEnv(t, map[string]string{"GIT_DIR": ".git"},
		"origin", "oci://localhost:1/does/not/matter")
	if err != nil {
		t.Fatalf("helper invocation failed: %v", err)
	}
	if stdout != "" {
		t.Errorf("expected no protocol output for a bare quit, got %q", stdout)
	}
}

// TestHelperInvocationWithAwkwardRemoteNames: a remote can be called almost
// anything, and only the reserved names are diverted.
func TestHelperInvocationWithAwkwardRemoteNames(t *testing.T) {
	for _, name := range []string{"origin", "upstream", "gc-mirror", "versions", "helpme"} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := runCLI(t, name, "oci://localhost:1/x/y"); err != nil {
				t.Errorf("remote %q was not treated as a helper invocation: %v", name, err)
			}
		})
	}
}

// TestCollidingRemoteNameIsRefused is the regression this dispatch exists for.
//
// Git invokes `git-remote-oci gc <url>` for a remote named "gc", which is the
// same argv as running the gc subcommand by hand. Dispatching to gc there ran a
// real garbage collection — repacking and pruning the registry — in response to
// a `git fetch`, and wrote its report onto stdout, which under git is the wire
// protocol. Every reserved name has some version of this; "version" and "help"
// merely printed onto the protocol stream instead.
func TestCollidingRemoteNameIsRefused(t *testing.T) {
	for _, name := range cli.ReservedNames() {
		t.Run(name, func(t *testing.T) {
			stdout, _, err := runCLIEnv(t, map[string]string{"GIT_DIR": ".git"},
				name, "oci://localhost:1/x/y")
			if err == nil {
				t.Fatalf("a remote named %q was accepted instead of refused", name)
			}
			if !strings.Contains(err.Error(), "git remote rename") {
				t.Errorf("error does not say how to fix it: %v", err)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error does not name the colliding remote %q: %v", name, err)
			}
			// Nothing may reach stdout: git is reading it as the protocol.
			if stdout != "" {
				t.Errorf("wrote %q to stdout, which git reads as the wire protocol", stdout)
			}
		})
	}
}

// TestSubcommandRunsWithoutGitDir: the collision check keys off GIT_DIR, so a
// subcommand run from an ordinary shell must dispatch rather than be refused.
func TestSubcommandRunsWithoutGitDir(t *testing.T) {
	stdout, _, err := runCLI(t, "version")
	if err != nil {
		t.Fatalf("version was not dispatched: %v", err)
	}
	if !strings.Contains(stdout, "test-version") {
		t.Errorf("version did not run: %q", stdout)
	}

	// The two-argument form is the shape that collides. Without GIT_DIR it
	// still reaches the subcommand, which then rejects the stray argument on
	// its own terms — a different failure from being refused as a remote.
	_, _, err = runCLI(t, "version", "oci://localhost:1/x/y")
	if err == nil {
		t.Fatal("version accepted a stray argument")
	}
	if strings.Contains(err.Error(), "git remote rename") {
		t.Errorf("refused as a colliding remote despite GIT_DIR being unset: %v", err)
	}
	if !strings.Contains(err.Error(), "takes no arguments") {
		t.Errorf("expected the arity error, got: %v", err)
	}
}

// TestForceSubcommandOverridesCollisionCheck covers the false positive: a user
// running a subcommand by hand in a shell that does export GIT_DIR.
func TestForceSubcommandOverridesCollisionCheck(t *testing.T) {
	_, _, err := runCLIEnv(t, map[string]string{
		"GIT_DIR":                   ".git",
		"GIT_REMOTE_OCI_SUBCOMMAND": "1",
	}, "version", "oci://localhost:1/x/y")
	if err == nil {
		t.Fatal("version accepted a stray argument")
	}
	// Reaching version's own arity check is the proof the override worked.
	if strings.Contains(err.Error(), "git remote rename") {
		t.Errorf("override did not take effect: %v", err)
	}
	if !strings.Contains(err.Error(), "takes no arguments") {
		t.Errorf("expected the arity error, got: %v", err)
	}
}

// TestVersionAndHelpRejectStrayArguments: every subcommand with a flag set
// validates its argument count, and these two used to be the exception.
func TestVersionAndHelpRejectStrayArguments(t *testing.T) {
	for _, name := range []string{"version", "--version", "-v", "help", "--help", "-h"} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := runCLI(t, name, "some-typo"); err == nil {
				t.Errorf("%s some-typo was accepted", name)
			}
		})
	}
}

func TestNoArguments(t *testing.T) {
	_, stderr, err := runCLI(t)
	if err == nil {
		t.Fatal("expected an error with no arguments")
	}
	if !strings.Contains(stderr, "git-remote-oci") {
		t.Errorf("usage was not printed:\n%s", stderr)
	}
}

func TestUnrecognisedSubcommand(t *testing.T) {
	_, _, err := runCLI(t, "frobnicate", "a", "b")
	if err == nil {
		t.Fatal("expected an error for an unknown subcommand")
	}
	if !strings.Contains(err.Error(), "unrecognised") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGCRequiresExactlyOneURL(t *testing.T) {
	if _, _, err := runCLI(t, "gc"); err == nil {
		t.Error("gc with no URL should fail")
	}
	if _, _, err := runCLI(t, "gc", "one", "two"); err == nil {
		t.Error("gc with two URLs should fail")
	}
}
