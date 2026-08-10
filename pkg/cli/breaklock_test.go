package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
)

// break-lock exists for one situation: a client died mid-push, its advisory
// lock is still on the registry, and the ref is blocked until the TTL expires.
// Everything about it was tested except that — argument handling, the name it
// was given, and the no-op on a ref that is not locked. Nothing had ever taken
// a lock and broken it, so `oci.BreakRefLock` was reached by no test at all.
//
// That is the worst place for a gap. A break-lock that quietly failed would be
// discovered by someone already having a bad day, who would conclude the lock
// was unbreakable and wait out the ten minutes.

// lockedRegistry serves a repository with an advisory lock held on
// refs/heads/main, and returns the URL a user would type.
func lockedRegistry(t *testing.T) (string, func(context.Context) bool) {
	t.Helper()

	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)

	if _, err := client.AcquireRefLock(context.Background(), "refs/heads/main", 10*time.Minute); err != nil {
		t.Fatalf("could not take the lock the test is about to break: %v", err)
	}

	// Asked of a separate client, because the interesting question is whether
	// the lock is gone from the *registry*, not from one process's memory.
	locked := func(ctx context.Context) bool {
		t.Helper()
		held, _, err := registrytest.Client(t, ts).IsLocked(ctx, "refs/heads/main")
		if err != nil {
			t.Fatalf("could not read the lock state: %v", err)
		}
		return held
	}
	if !locked(context.Background()) {
		t.Fatal("the lock did not take, so nothing below would be testing anything")
	}
	return registrytest.URL(ts), locked
}

// TestBreakLockRefusesWithoutForce: taking a lock away from whoever holds it is
// the point of the command and also how two clients end up writing at once, so
// it has to be asked for explicitly.
func TestBreakLockRefusesWithoutForce(t *testing.T) {
	url, locked := lockedRegistry(t)

	_, _, err := runCLI(t, "break-lock", url, "refs/heads/main")
	if err == nil {
		t.Fatal("break-lock released a held lock without --force")
	}
	// The message has to say what to do next and when the lock lapses on its
	// own; a bare refusal leaves the user with no move.
	for _, want := range []string{"--force", "locked by"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	if !locked(context.Background()) {
		t.Error("a refused break-lock released the lock anyway")
	}
}

// TestBreakLockWithForceReleasesTheLock is the command doing its job, and the
// path no test reached before.
func TestBreakLockWithForceReleasesTheLock(t *testing.T) {
	url, locked := lockedRegistry(t)

	stdout, stderr, err := runCLI(t, "break-lock", "--force", url, "refs/heads/main")
	if err != nil {
		t.Fatalf("break-lock --force failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "broke the lock") {
		t.Errorf("break-lock did not report what it did: %q", stdout)
	}

	// The assertion that matters: the registry no longer holds it.
	if locked(context.Background()) {
		t.Fatal("break-lock reported success and the lock is still held")
	}

	// And the ref is workable again — a second break-lock finds nothing to do
	// rather than erroring, which is what the next person to try will see.
	stdout, stderr, err = runCLI(t, "break-lock", "--force", url, "refs/heads/main")
	if err != nil {
		t.Fatalf("break-lock on the now-unlocked ref: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "not locked") {
		t.Errorf("expected it to report the ref as unlocked, got: %q", stdout)
	}
}
