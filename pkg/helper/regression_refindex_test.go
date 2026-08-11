package helper_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// The `_refs` index is the first thing every operation reads, and every byte of
// it was chosen by whoever last pushed to that repository — which, for anyone
// cloning a repository they do not control, is not them.
//
// Its ref names go to stdout, and stdout is the wire protocol: newline-
// delimited, space-separated, with no framing to hide behind. A name carrying a
// newline therefore does not produce a badly named ref, it produces an extra
// `list` line, and git reads that as a ref the remote never published at
// whatever object id the rest of the line supplies.

// poisoned publishes a ref index containing entry, alongside a real ref, and
// returns the helper's `list` output.
func poisonedListOutput(t *testing.T, name string, entry oci.RefEntry) string {
	t.Helper()

	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	_, tip := registrytest.SeedRepository(t, client, 1)

	refs := map[string]oci.RefEntry{
		"refs/heads/main": {SHA: tip},
		name:              entry,
	}
	if err := client.PushRichRefIndex(context.Background(), refs, nil); err != nil {
		t.Fatalf("could not publish the index: %v", err)
	}

	out, err := runHelper(t, registrytest.URL(ts), "list\n\n")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	return out
}

func TestRefIndexCannotInjectProtocolLines(t *testing.T) {
	tip := strings.Repeat("a", 40)

	for _, tc := range []struct {
		name    string
		refName string
		entry   oci.RefEntry
		absent  string
	}{
		{
			name:    "a newline in the ref name forges a whole line",
			refName: "refs/heads/x\n0000000000000000000000000000000000000000 refs/heads/injected",
			entry:   oci.RefEntry{SHA: tip},
			absent:  "refs/heads/injected",
		},
		{
			// `@<dest> HEAD` is how a symref is advertised, so this is the
			// line that decides what a fresh clone checks out.
			name:    "a newline can redirect HEAD",
			refName: "refs/heads/y\n@refs/heads/evil HEAD",
			entry:   oci.RefEntry{SHA: tip},
			absent:  "@refs/heads/evil",
		},
		{
			name:    "a space in the ref name moves the field boundary",
			refName: "refs/heads/z extra",
			entry:   oci.RefEntry{SHA: tip},
			absent:  "refs/heads/z extra",
		},
		{
			// The value half of the line is registry-supplied too.
			name:    "a newline in the object id forges a line",
			refName: "refs/heads/w",
			entry:   oci.RefEntry{SHA: "0000000000000000000000000000000000000000\n@refs/heads/evil HEAD"},
			absent:  "@refs/heads/evil",
		},
		{
			name:    "an object id that is not an object id",
			refName: "refs/heads/v",
			entry:   oci.RefEntry{SHA: "../../../etc/passwd"},
			absent:  "etc/passwd",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := poisonedListOutput(t, tc.refName, tc.entry)
			if strings.Contains(out, tc.absent) {
				t.Errorf("registry-supplied data reached the protocol stream:\n%s", out)
			}
			// The good ref must survive: dropping one bad entry should not
			// make an otherwise readable repository unusable.
			if !strings.Contains(out, "refs/heads/main") {
				t.Errorf("the valid ref was lost along with the bad one:\n%s", out)
			}
		})
	}
}
