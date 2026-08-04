package helper

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// configuredRepo creates a repository holding the given git config entries and
// makes it the working directory.
func configuredRepo(t *testing.T, entries map[string]string) {
	t.Helper()

	// Global and system scope are excluded so the test cannot be swayed by the
	// developer's own ociremote.* settings, the same way GIT_DIR must not be
	// inherited by the dispatch tests.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", ".")
	for k, v := range entries {
		run("config", k, v)
	}
	t.Chdir(dir)
}

// TestNewHelperAppliesDefaults pins what the tunables were before they became
// tunable, so a change to a default is a deliberate edit rather than a drift.
func TestNewHelperAppliesDefaults(t *testing.T) {
	configuredRepo(t, nil)

	h, err := NewHelper("origin", "oci://localhost:1/x/y", strings.NewReader(""), &strings.Builder{})
	if err != nil {
		t.Fatalf("NewHelper: %v", err)
	}

	if h.transferWorkers != defaultTransferWorkers {
		t.Errorf("transferWorkers = %d, want %d", h.transferWorkers, defaultTransferWorkers)
	}
	if h.blobWorkers != defaultBlobWorkers {
		t.Errorf("blobWorkers = %d, want %d", h.blobWorkers, defaultBlobWorkers)
	}
	if h.pushLockTTL != defaultPushLockTTL {
		t.Errorf("pushLockTTL = %v, want %v", h.pushLockTTL, defaultPushLockTTL)
	}
	if h.ociClient.RefsIndexLockTTL != oci.DefaultRefsIndexLockTTL {
		t.Errorf("RefsIndexLockTTL = %v, want %v",
			h.ociClient.RefsIndexLockTTL, oci.DefaultRefsIndexLockTTL)
	}
}

// TestNewHelperUsesTheRemoteName is why NewHelper takes a remote name at all.
// It was an unused parameter carrying a "reserved for future use" comment;
// per-remote configuration is that use.
func TestNewHelperUsesTheRemoteName(t *testing.T) {
	configuredRepo(t, map[string]string{
		"ociremote.concurrency":           "8",
		"ociremote.pushlockttl":           "4m",
		"remote.slowhost.ociconcurrency":  "2",
		"remote.slowhost.ocipushlockttl":  "30m",
		"remote.slowhost.ociindexlockttl": "20m",
	})

	slow, err := NewHelper("slowhost", "oci://localhost:1/x/y", strings.NewReader(""), &strings.Builder{})
	if err != nil {
		t.Fatalf("NewHelper(slowhost): %v", err)
	}
	if slow.transferWorkers != 2 {
		t.Errorf("slowhost transferWorkers = %d, want the per-remote 2", slow.transferWorkers)
	}
	if slow.pushLockTTL != 30*time.Minute {
		t.Errorf("slowhost pushLockTTL = %v, want the per-remote 30m", slow.pushLockTTL)
	}
	if slow.ociClient.RefsIndexLockTTL != 20*time.Minute {
		t.Errorf("slowhost RefsIndexLockTTL = %v, want the per-remote 20m",
			slow.ociClient.RefsIndexLockTTL)
	}

	// A different remote in the same repository falls back to the repository
	// scope, not to slowhost's values.
	other, err := NewHelper("origin", "oci://localhost:1/x/y", strings.NewReader(""), &strings.Builder{})
	if err != nil {
		t.Fatalf("NewHelper(origin): %v", err)
	}
	if other.transferWorkers != 8 {
		t.Errorf("origin transferWorkers = %d, want the repository-wide 8", other.transferWorkers)
	}
	if other.pushLockTTL != 4*time.Minute {
		t.Errorf("origin pushLockTTL = %v, want the repository-wide 4m", other.pushLockTTL)
	}
	if other.ociClient.RefsIndexLockTTL != oci.DefaultRefsIndexLockTTL {
		t.Errorf("origin RefsIndexLockTTL = %v, want the default",
			other.ociClient.RefsIndexLockTTL)
	}
}

// TestEnvironmentBeatsConfigForCompression: OCI_COMPRESSION predates the config
// keys and a one-off override must not require editing a config file.
func TestEnvironmentBeatsConfigForCompression(t *testing.T) {
	configuredRepo(t, map[string]string{"ociremote.compression": "zstd"})

	h, err := NewHelper("origin", "oci://localhost:1/x/y", strings.NewReader(""), &strings.Builder{})
	if err != nil {
		t.Fatalf("NewHelper: %v", err)
	}
	if h.ociClient.Compression != "zstd" {
		t.Fatalf("config was not applied: Compression = %q", h.ociClient.Compression)
	}

	t.Setenv("OCI_COMPRESSION", "gzip")
	h, err = NewHelper("origin", "oci://localhost:1/x/y", strings.NewReader(""), &strings.Builder{})
	if err != nil {
		t.Fatalf("NewHelper: %v", err)
	}
	if h.ociClient.Compression != "gzip" {
		t.Errorf("Compression = %q, want the environment to win with gzip", h.ociClient.Compression)
	}
}
